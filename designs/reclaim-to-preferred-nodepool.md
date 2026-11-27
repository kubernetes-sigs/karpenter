# Reclaim to Preferred NodePool

**Author:** Adir Atias (@atiasadir)
**Issue:** [#3704](https://github.com/kubernetes-sigs/karpenter/issues/3704)

Move nodes from a lower-weight NodePool back to a higher-weight NodePool once that NodePool can launch capacity again.

This picks up [kubernetes-sigs/karpenter#1829](https://github.com/kubernetes-sigs/karpenter/issues/1829) ("consolidationPolicy: Underweight"). A maintainer agreed weighted preference matters, asked for a concrete scenario and labeled it `priority/awaiting-more-evidence`; it was then closed by the triage bot as rotten. This RFC brings that evidence (open user reports, a reproduction, a gated prototype, and KWOK runs) and a design.

## Motivation

Karpenter launches from the highest-weight NodePool that fits ([`provisioner.go` L313](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/controllers/provisioning/provisioner.go#L313)). When that NodePool cannot launch (capacity errors, quota, its limits), providers mark the offering unavailable for a TTL and Karpenter launches from a lower-weight NodePool. That is the right short-term behavior.

Nothing moves pods back. Weight is a statement of preference, but it is only read at launch time. Once the preferred NodePool recovers, nodes stay in the lower-weight NodePool until they become empty, expire or drift. Consolidation only helps when the preferred placement is also cheaper and fits in one replacement node, and it does nothing under `consolidationPolicy: WhenEmpty`.

Users keep hitting this:

- [aws/karpenter-provider-aws#7832](https://github.com/aws/karpenter-provider-aws/issues/7832) (open, reopened 12 Nov 2026 as `triage/upstream-issue`): on-demand nodes never return to spot. A maintainer reproduced it on v1.15.1: with a preferred hostname anti-affinity, consolidation plans one node per pod and single-node consolidation rejects any plan with more than one replacement ([`consolidation.go` L188-L193](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/controllers/disruption/consolidation.go#L188-L193)), tracked in kubernetes-sigs/karpenter#1442.
- [aws/karpenter-provider-aws#3480](https://github.com/aws/karpenter-provider-aws/issues/3480) (23 comments) and aws/karpenter-provider-aws#1718: no return to spot after an on-demand fallback.
- [aws/karpenter-provider-aws#8889](https://github.com/aws/karpenter-provider-aws/issues/8889): an on-demand-only fallback NodeClaim cannot be consolidated to spot, because its requirements allow only on-demand.
- [aws/karpenter-provider-aws#6721](https://github.com/aws/karpenter-provider-aws/issues/6721) (26 upvotes): prefer an instance generation over price. Closed with a pointer to NodeOverlay `priceAdjustment`; a later user reply says the intent is a preference order, not a cost change.
- kubernetes-sigs/karpenter#1653: SpotToSpotConsolidation needs 15 cheaper instance types, so many spot moves never happen.
- The workaround in #1829 was `expireAfter: 4h` on the fallback NodePool: hours of lag, and the pool cannot host long-lived workloads.

### Use Cases

Not about price first:

1. **Home region** ([Azure/karpenter-provider-azure#2088](https://github.com/Azure/karpenter-provider-azure/pull/2088), multi-region provisioning design by Hen Goueta). When the home region cannot supply quota or capacity, NodePools in other regions reuse whatever quota and capacity exist there, which the provider watches. When home quota or capacity returns, workloads should come home for latency and egress cost. #2088 notes that NodePool weights do not move running workloads.
2. **Preferred family or generation.** A NodePool for the newest generation (weight 50) and one for the previous generation (weight 40). The preferred family hits its per-family quota or runs out of capacity; the older family takes the load. When quota is raised or capacity returns, users want the newer generation, even though it is not cheaper (aws#6721).
3. **On-demand preferred for stability.** A latency-sensitive service prefers on-demand (weight 50) and accepts spot (weight 40) only when on-demand is short. When on-demand is back, users want to leave spot. Consolidation pushes the other way (spot is cheaper).

Also price-aligned:

4. **Spot preferred, on-demand as overflow, in separate NodePools** (#1829, aws#3480, aws#8889). Consolidation already replaces on-demand with cheaper spot when the replacement allows spot ([`consolidation.go` L236-L242](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/controllers/disruption/consolidation.go#L236-L242)) and the move fits one node. Reclaim covers the rest: `WhenEmpty`, an on-demand-only fallback NodePool (aws#8889), and moves that need more than one node.
5. **Capacity reservation.** With `ReservedCapacity` (beta, on by default), reserved offerings are priced near zero, so consolidation already tends to move pods into a freed reservation when consolidation is enabled and the move fits one node. Reclaim covers `WhenEmpty` and N-for-1 moves.

### Non-Goals

- Changing how launches pick a NodePool, or providers' unavailable-offering TTLs.
- Predicting capacity. Reclaim reacts to what the provider reports; it does not probe the cloud.
- Preferences inside a single NodePool (for example spot and on-demand in one NodePool, as in aws#7832). That is consolidation's job and is tracked in #1442; Reclaim acts on NodePool weight.
- Static (`replicas`) NodePools and nodes Karpenter does not manage.

## Proposal

A new disruption method, `Reclaim`, behind the alpha feature gate `ReclaimToPreferredNodePool`. A NodePool opts in with `spec.disruption.reclaimPolicy`. Reclaim replaces a node of that NodePool only when the scheduling simulation places **all** of its pods on capacity from strictly higher-weight NodePools (new or existing nodes). Replacements launch before the node drains, as for drift.

### Proposed Spec

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: previous-generation
spec:
  weight: 40
  disruption:
    reclaimPolicy: WhenPreferredAvailable   # new; Never (default) | WhenPreferredAvailable
    reclaimAfter: 15m                       # new; minimum node age before reclaim
    budgets:
      - nodes: "10%"
        reasons: ["Reclaimed"]              # new DisruptionReason
```

```go
type ReclaimPolicy string

const (
    ReclaimPolicyNever                  ReclaimPolicy = "Never"
    ReclaimPolicyWhenPreferredAvailable ReclaimPolicy = "WhenPreferredAvailable"
)

// In Disruption:
ReclaimPolicy *ReclaimPolicy   `json:"reclaimPolicy,omitempty" hash:"ignore"`
ReclaimAfter  *metav1.Duration `json:"reclaimAfter,omitempty" hash:"ignore"`

const DisruptionReasonReclaimed DisruptionReason = "Reclaimed"
```

- The fields sit under `spec.disruption`, outside `spec.template`. [`NodePool.Hash()`](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/apis/v1/nodepool.go#L342) hashes only `spec.template`, so setting or changing them never drifts nodes. They also carry `hash:"ignore"`, like `budgets`, in case the hash scope widens.
- `Reclaimed` joins the budget reasons enum ([`nodepool.go` L182](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/apis/v1/nodepool.go#L182): `Underutilized`, `Empty`, `Drifted`, `Unhealthy`).

### How It Works

1. **Candidates.** Nodes of NodePools with `reclaimPolicy: WhenPreferredAvailable`, older than `reclaimAfter`, not blocked by `karpenter.sh/do-not-disrupt`, with at least one reschedulable pod, and with a higher-weight NodePool in the cluster. Oldest first, one candidate per command.
2. **No separate pre-filter.** The simulation uses the offerings the provider reports and the NodePool limits. An offering in its unavailable TTL, or a preferred NodePool at its limits, makes the simulation place pods back in the candidate's own NodePool, and Reclaim skips the node. Provider source (pinned to Azure/karpenter-provider-azure `c12bd53` and aws/karpenter-provider-aws v1.15.3, `8c91894`):
   - Azure sets `Available: !unavailableOfferings.IsUnavailable(...)` in [`instancetypes.go`](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instancetype/instancetypes.go) (on-demand [L468](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instancetype/instancetypes.go#L468)/[L486](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instancetype/instancetypes.go#L486), spot [L506](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instancetype/instancetypes.go#L506)/[L518](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instancetype/instancetypes.go#L518)). AWS sets `Available: ... && !isUnavailable && ...` in [`base_resolver.go`](https://github.com/aws/karpenter-provider-aws/blob/8c91894e993eda682f0785091b79a93da68f9830/pkg/providers/instancetype/offering/base_resolver.go) ([L106](https://github.com/aws/karpenter-provider-aws/blob/8c91894e993eda682f0785091b79a93da68f9830/pkg/providers/instancetype/offering/base_resolver.go#L106), [L135](https://github.com/aws/karpenter-provider-aws/blob/8c91894e993eda682f0785091b79a93da68f9830/pkg/providers/instancetype/offering/base_resolver.go#L135)). Both re-key their instance-type cache on the unavailable-offerings sequence number (Azure [L299](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instancetype/instancetypes.go#L299); AWS [L75-L79](https://github.com/aws/karpenter-provider-aws/blob/8c91894e993eda682f0785091b79a93da68f9830/pkg/providers/instancetype/offering/base_resolver.go#L75-L79)).
   - AWS uses one TTL, 3 min ([`cache.go` L37](https://github.com/aws/karpenter-provider-aws/blob/8c91894e993eda682f0785091b79a93da68f9830/pkg/cache/cache.go#L37)).
   - Azure sets it per failure reason in [`commonerrorhandlers.go`](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go) (constants [L55-L76](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go#L55-L76)): allocation failures 1 h (zonal [L273-L274](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go#L273-L274), regional [L299](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go#L299), overconstrained [L317](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go#L317)/[L334](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go#L334)); family quota 1 h at limit 0, else 10 min ([L206](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go#L206)/[L208](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go#L208)); `InvalidAvailabilityZone` 4 h; `SKUNotAvailable` 1 h spot, 23 h on-demand. Regional vCPU quota errors are not cached ([`handleRegionalQuotaError` L340-L366](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instance/offerings/commonerrorhandlers.go#L340-L366)), except for capacity reservation members.
   - Azure also watches family quota for the cluster's region: it polls `Microsoft.Compute/locations/usages` every 10 min ([`controller.go` L35](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/controllers/quota/controller.go#L35)), adjusts it locally on creates and deletes, and `applyQuota` sets on-demand offerings of families without quota to `Available: false` ([L334](https://github.com/Azure/karpenter-provider-azure/blob/c12bd5326ea9edaa96dab61d8dbf563e22997b3e/pkg/providers/instancetype/instancetypes.go#L334)). That overlay only clears availability, on top of the cache above.
3. **Simulate.** Reuse `SimulateScheduling` (`pkg/controllers/disruption/helpers.go`), which orders NodePools by weight and considers existing nodes. Proceed only if every reschedulable pod lands on a node whose NodePool weight is greater than the candidate's. Pods placed on unmanaged nodes, or on NodePools of equal or lower weight, block the reclaim (`ReclaimBlocked`).
4. **N-for-1 is allowed.** Like drift, and unlike single-node consolidation, Reclaim accepts a plan with several replacement NodeClaims, as long as all of them are in strictly higher-weight NodePools. This is the aws#7832 case: with a preferred hostname anti-affinity the simulation plans one node per pod. Reclaim does not relax preferences; the target placement satisfies them, and relaxing is left to consolidation (#1442). The cost is bounded: one candidate per command, the `Reclaimed` budget, `reclaimAfter`, and replacements cannot outnumber the candidate's reschedulable pods.
5. **Execute.** Same as drift: launch replacements, wait for initialization, taint and drain. If a replacement fails to launch or initialize, the queue rolls back and un-taints the candidate, and the provider marks the offering unavailable again.
6. **Ordering.** After `Drift`, before consolidation (`EventualDisruptionClass`).

### Interaction with Existing Features

| Feature | Interaction |
|---|---|
| Budgets | New `Reclaimed` reason; the default budget applies. |
| PDBs, do-not-disrupt | Same as drift. Pods behind fully blocking PDBs are left out of the simulation. |
| Consolidation | Without a guard, consolidation could move pods back to a cheaper lower-weight NodePool and undo a reclaim (use case 3). See below. |
| Drift | `weight` is outside `spec.template`, so changing it never drifts nodes. Reclaim does not change that. |
| Unavailable offerings | Offerings in their TTL are `Available: false`; the simulation does not place pods on them. Reclaim does not clear or bypass the provider cache. |
| NodeOverlay | Reclaim reads the same (overlay-adjusted) instance types. It decides by weight, not price, so overlays do not change whether it acts. |
| Terminate-first (`designs/terminate-first-disruption.md`) | Not used; Reclaim is always replace-first. |

#### Consolidation guard

A NodePool that sets `reclaimPolicy: WhenPreferredAvailable` declares itself a lower-preference NodePool. Consolidation respects that in one direction: when it simulates moving a candidate's pods, it must not place them on capacity in a NodePool that has a lower weight than the candidate's NodePool **and** sets `reclaimPolicy: WhenPreferredAvailable`. Everything else is unchanged. This prevents a loop with no new field and no change for users who don't opt in. It ships behind the same feature gate.

### Observability

- `karpenter_disruption_decisions_total{reason="reclaimed"}` (existing metric, new label value).
- Events: `Reclaimed` on the node and NodeClaim; `ReclaimBlocked` with the blocking reason.
- Metric: `karpenter_nodepool_reclaimable_nodes{nodepool}`.

### Edge Cases

- **Flapping.** The preferred NodePool is still short; a reclaim replacement fails and the offering is marked unavailable for its TTL. Cost: one failed launch per TTL per offering, about one per 3 min on AWS and about one per hour on Azure for allocation and zero-quota failures (10 min for low family quota). `reclaimAfter` and budgets limit it further.
- **Recovery lag.** Reclaim acts once the simulation sees the preferred offering available. On AWS that is up to 3 min after the last failure. On Azure, a family held back only by watched quota returns within one 10 min poll, or at once after a local delete; a family that returned a quota error stays blocked for that error's TTL even if watched quota recovers sooner; capacity errors block for 1 h. See Open Question 4.
- **Uncached failures (provider dependency).** Reclaim relies on the provider making the failed offering unavailable. Azure does not cache regional vCPU quota errors: in a single region nothing else can launch after one, so caching adds nothing ([Azure/karpenter-provider-azure#1433](https://github.com/Azure/karpenter-provider-azure/pull/1433) dropped that caching in review and was later closed as superseded by #1551). With NodePools in other regions (#2088) this changes: each region's watched regional quota must become a placement input, so a home region without regional quota is not offered.
- **Partial fit.** Some pods fit only the lower-weight NodePool. The simulation fails and the node is skipped.
- **Higher price.** Expected in use cases 1-3. `weight` expresses preference over price.
- **Cross-region moves.** Pods with zonal persistent volumes cannot move across regions; the simulation already fails them on volume topology.

## Alternatives Considered

### Alternative 1: Consolidation (`Underutilized`) as-is

Covers the cheaper-and-fits-one-node part of use cases 4-5 (on-demand to spot narrowing, near-zero reserved prices). It does not act under `WhenEmpty`, rejects plans with more than one replacement (aws#7832, #1442), cannot move an on-demand-only NodeClaim to spot (aws#8889), and pushes the opposite way in use cases 1-3 where the preferred placement costs more.

### Alternative 2: Why not NodeOverlay `priceAdjustment`?

NodeOverlay (`v1alpha1`, gate `NodeOverlay`, alpha and off by default in both v1.15.1 and main, [`options.go` L111-L114](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/operator/options/options.go#L111-L114)) can make the preferred instance types look cheaper, and its design says adjusted prices feed consolidation. Maintainers suggested it for aws#6721. It does not cover this case:

- It still goes through consolidation, so preferred constraints still block it (aws#7832) and nothing happens under `WhenEmpty`.
- Synthetic prices change every price-based decision and any cost reporting built on them, not just the move back.
- Penalties must stay correctly ordered across instance types, zones, regions and moving spot prices. A penalty large enough to always win also hides real price differences inside the preferred set.
- A home region is a latency and egress preference, not a price (use case 1). A user on aws#6721 made the same point: the intent is an order, not a cost change.

NodeOverlay and Reclaim compose: overlays shape prices within a NodePool; weight plus Reclaim expresses order across NodePools.

### Alternative 3: Descheduler

Evicts pods but cannot launch capacity first. Pods can land back on the same node or go pending.

### Alternative 4: External controller that triggers drift

An out-of-tree controller marks lower-weight NodeClaims drifted one at a time (by changing their `karpenter.sh/nodepool-hash` annotation) once the preferred NodePool looks healthy. It works without core changes and was tested on KWOK (see [Prototype](#prototype)), but it cannot see the simulation or the provider's unavailable-offerings cache, so it guesses when to try, can waste a replacement that lands back in the lower-weight NodePool, and depends on an internal annotation.

### Alternative 5: `expireAfter` on the lower-weight NodePool

The #1829 workaround. Moves nodes back hours late, moves them even when the preferred NodePool is still short, and rules out long-lived workloads on that NodePool.

### Alternative 6: Make `weight` changes drift nodes

Would move every node on any weight edit, whether or not the preferred NodePool can launch.

## Backward Compatibility

The default is `reclaimPolicy: Never` (nil), so behavior does not change. The new fields are outside `spec.template` and `hash:"ignore"`, so adding them does not drift nodes; the NodePool hash version is unchanged. `Reclaimed` is added to the budget reasons enum; existing budgets are unchanged.

## Graduation Criteria

- **Alpha:** gate `ReclaimToPreferredNodePool` off by default. Fake-provider and KWOK tests pass.
- **Beta:** production use reported on at least one provider; flapping stays within one failed launch per TTL per offering; consolidation guard in place.
- **GA:** no open issues on the consolidation interaction or provider dependencies; gate removed.

### Test Plan

- **Fake provider:** two NodePools (weights 100 and 10). Variants: preferred at its limit, then raised; `reclaimPolicy` unset or `Never`; `reclaimAfter`; `Reclaimed` budget of 0; node already in the highest-weight NodePool; do-not-disrupt pod; preferred hostname anti-affinity (N-for-1); hash unchanged when the new fields change; method registered only with the gate on and after `Drift`.
- **KWOK:** end to end in kind, with the preferred NodePool's `limits` as one lever and a test-only capacity-error toggle in the KWOK provider (launches of listed instance types fail with `InsufficientCapacityError` and their offerings are reported unavailable for a TTL) as the other.

## Open Questions

1. Should consolidation be blocked from undoing a reclaim? Proposed answer: yes, with the one-directional [consolidation guard](#consolidation-guard).
2. NodePool field or a cluster-level setting?
3. Should N-for-1 replacement have a hard cap beyond the pod count and budget?
4. Should providers clear an unavailable offering early on evidence of recovery (watched quota showing room, or a successful launch of the same offering from another NodePool)? Provider change, outside this RFC.
5. Multi-node reclaim (batched candidates) in alpha, or single-node only?

## Prototype

A prototype on v1.15.1 can follow as an implementation PR once this RFC is accepted. It adds the API fields, the `Reclaimed` reason, the feature gate (default off), a `Reclaim` disruption method (about 150 lines) registered after `Drift`, and the test-only KWOK capacity-error toggle. It does not yet include the consolidation guard, the events, the metric or a test for the anti-affinity N-for-1 case.

**Unit tests.** 11 new fake-provider specs pass, and the existing `pkg/apis/...`, `pkg/operator/options`, `pkg/controllers/disruption`, `pkg/controllers/nodeclaim/disruption` and `pkg/controllers/nodepool/hash` suites pass.

**KWOK, NodePool limits (kind, gate on).** Preferred NodePool weight 50 with a 4-CPU limit; lower-weight NodePool weight 40 with `reclaimPolicy: WhenPreferredAvailable`, `reclaimAfter: 1m` and a `Reclaimed` budget of 1; a 4-replica, 1-CPU workload with a PDB. With the preferred NodePool at its limit, 2 pods stayed in the lower-weight NodePool for 3 minutes and no `Reclaimed` command was issued. After the limit was raised to 16, two `Reclaimed` replace commands ran one at a time (11 s apart); within 30 s all 4 pods ran in the preferred NodePool.

**KWOK, capacity errors (kind, gate on).** Same NodePools, with launches of the preferred instance type failing with `InsufficientCapacityError` and a 30 s unavailable TTL. All 4 pods landed in the lower-weight NodePool. During a 4-minute hold Reclaim issued 5 `Reclaimed` commands; each replacement hit the error and was rolled back, one per TTL. After the errors stopped, 4 more `Reclaimed` commands moved all 4 pods to the preferred NodePool within about 5 minutes.

The external-controller approach (Alternative 4), on the same capacity-error case, aborted 3 rounds while errors were on and completed 4 after they stopped.
