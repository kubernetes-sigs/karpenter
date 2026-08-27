# NodePool Rollout Status

## Motivation

Karpenter exposes drift per-NodeClaim, via the `Drifted` status condition. There is no aggregate signal on the NodePool that answers the question an operator or an external orchestrator asks after pushing a change: *"has this NodePool finished rolling out the spec I just applied?"*

Answering it today requires listing NodeClaims, grouping them by `karpenter.sh/nodepool`, and aggregating their conditions. Every consumer that gates on a single resource's status — Argo CD health checks, kro `readyWhen` expressions, `kstatus`, `kubectl wait` — is structurally unable to do that, because their evaluation is scoped to the one resource they are looking at. The workaround is an out-of-band job that re-implements the aggregation Karpenter already performs internally ([#3071](https://github.com/kubernetes-sigs/karpenter/issues/3071)).

Core workload controllers solve this by reporting rollout accounting on the parent: a Deployment reports `replicas`/`updatedReplicas`/`readyReplicas` plus `observedGeneration`, a DaemonSet reports `desiredNumberScheduled`/`updatedNumberScheduled`, and Cluster API reports `upToDateReplicas` on MachineDeployments alongside an `UpToDate` condition on Machines. `kubectl rollout status` and essentially all GitOps tooling are built on that convention. NodePool is already partway there: it aggregates `status.resources` and `status.nodes`, and `status.nodes` is the `statuspath` of its scale subresource, so it already occupies the position `status.replicas` does. This RFC extends that accounting to rollout progress.

Earlier attempts at this are [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) and [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177). The difference from these is that we do not propose surfacing "drift" on the NodePool. We propose surfacing *how many of a NodePool's nodes were provisioned from its current spec revision*. Drift is the mechanism that eventually makes those numbers converge; the revision is the contract consumers gate on.

### Use Cases

1. **GitOps sync sequencing (Argo CD).** A NodePool is deployed by an Argo Application. A custom Lua health check must report `Progressing` while the change propagates and `Healthy` once it has, so that downstream Applications sync in order. The check can read only `NodePool.status`.
2. **Composite APIs (kro).** A `ResourceGraphDefinition` wraps NodePool + NodeClass into one custom API and evaluates `readyWhen` CEL against the resources in the graph. NodeClaims are not in the graph — their names and count are not known at authoring time — so the instance flips to ready as soon as the NodePool is admitted rather than when the replacement completes.
3. **Threshold-based gates.** Both of the above want tolerance ("settled at ≥ 90% up to date") rather than an all-or-nothing boolean, because disruption budgets make node rollout deliberately gradual and a single stuck node should not block a pipeline indefinitely.
4. **Fleet-wide rollout dashboards.** "Are we fully onto the new AMI?" — a time-series question over *all* drift vectors, including out-of-band ones. This is a different question from 1–3 and we suggest using metrics rather than status. See [Which drift vectors count](#which-drift-vectors-count).

### Non-Goals

- **Aggregating arbitrary NodeClaim conditions onto the NodePool.** #3108 proposed a generic `status.nodeClaimConditions[]` list of `{conditionType, count}`. That makes an open, provider- and version-extensible set of condition types part of the NodePool API with no defined semantics per entry, and no way to version or deprecate individual entries. We propose named fields with defined meanings instead.
- **Policy in the API.** No thresholds, settling windows, or "rollout paused/complete" state machine. Karpenter reports counts; consumers apply their own policy.
- **Changing disruption behavior.** Nothing here gates, throttles, or reorders drift.
- **Covering cloud-provider and out-of-band drift in v1 counts.** See [Extending beyond NodePool-attributable drift](#extending-beyond-nodepool-attributable-drift) for the path to adding it.
- **Changing `status.resources`.** Capacity accounting stays on cluster state, untouched. The one existing field this RFC does change is `status.nodes`, and only in what it counts — see [Redefining `status.nodes`](#redefining-statusnodes).

## Proposal

### Proposed Spec

Four additive, read-only fields on `NodePool.status`, plus a redefinition of the existing `status.nodes` so that it can serve as their denominator (see [Redefining `status.nodes`](#redefining-statusnodes)):

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
  generation: 12
status:
  observedGeneration: 12          # NEW: the generation the counts below were derived from
  nodes: 21                       # REDEFINED: NodeClaims owned by this NodePool
  upToDateNodes: 14               # NEW: of those, provisioned from the current NodePool revision
  readyNodes: 18                  # NEW: of those, whose NodeClaim Ready condition is True
  upToDateAndReadyNodes: 12       # NEW: of those, both
  nodeClassObservedGeneration: 4  # exists today
  resources: {...}
  conditions:
    - type: NodesUpToDate         # NEW
      status: "False"
      reason: RolloutInProgress
      message: 14/21 nodes are up to date
      observedGeneration: 12
```

```go
type NodePoolStatus struct {
    // ... existing fields ...

    // ObservedGeneration is the generation of the NodePool spec that the node counts
    // below were computed against. Consumers gating on rollout progress must ignore those
    // counts when this does not equal metadata.generation.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Nodes is the count of NodeClaims owned by this NodePool, including NodeClaims that
    // have not yet launched or registered a Node and NodeClaims that are terminating.
    // +kubebuilder:default:=0
    // +optional
    Nodes *int64 `json:"nodes"`

    // UpToDateNodes is the count of nodes owned by this NodePool that were provisioned
    // from the NodePool's current spec.template revision. The difference between Nodes and
    // UpToDateNodes is the number of nodes that Karpenter will replace to complete the
    // rollout of the current NodePool spec.
    // +kubebuilder:default:=0
    // +optional
    UpToDateNodes *int64 `json:"upToDateNodes"`

    // ReadyNodes is the count of nodes owned by this NodePool whose NodeClaim Ready
    // condition is True, meaning they have launched, registered a Node, and initialized.
    // This reports whether a node successfully came up, not whether it is currently
    // healthy: the underlying conditions do not revert if the Node later goes NotReady.
    // +kubebuilder:default:=0
    // +optional
    ReadyNodes *int64 `json:"readyNodes"`

    // UpToDateAndReadyNodes is the count of nodes owned by this NodePool that are counted
    // by both UpToDateNodes and ReadyNodes. A rollout of the current NodePool spec is
    // complete when this equals Nodes.
    // +kubebuilder:default:=0
    // +optional
    UpToDateAndReadyNodes *int64 `json:"upToDateAndReadyNodes"`
}
```

Plus one status condition, `NodesUpToDate`, `True` when `upToDateNodes == nodes`, with `observedGeneration` set. The name is chosen over Deployment's `Progressing` because `Progressing` inverts the polarity of every other Karpenter condition, where `True` is the settled state; the `Nodes` prefix matches the existing `NodeClassReady` and `NodeRegistrationHealthy`. It is deliberately **not** added to the NodePool's `Ready` aggregate (`status.NewReadyConditions(ConditionTypeValidationSucceeded, ConditionTypeNodeClassReady)`) — a pool mid-rollout is healthy, not unready, and folding this in would silently change `Ready` semantics for every existing consumer.

Consumers gate as follows. Argo CD:

```lua
if obj.status.observedGeneration ~= obj.metadata.generation then
  return { status = "Progressing", message = "NodePool status is stale" }
end
if obj.status.upToDateAndReadyNodes < obj.status.nodes * 0.9 then
  return { status = "Progressing", message = "rolling out" }
end
return { status = "Healthy" }
```


A rollout gate needs both revision agreement and workload readiness, which is why `upToDateNodes` and `readyNodes` are reported separately in the same way Deployment separates `updatedReplicas` from `readyReplicas`. Karpenter creates a replacement before terminating the node it replaces, so there is a window near the end of a rollout where every remaining node is up to date but the newest ones have not registered or initialized yet. A gate on `upToDateNodes` alone would report Healthy during that window and allow the next Argo Application in the overall sequence to sync prematurely.

`upToDateAndReadyNodes` is reported because that intersection cannot be derived from the other two counts. Knowing that 14 of 21 nodes are up to date and 18 of 21 are ready says nothing about how many are both: anywhere from 11 to 14, depending on whether the unready nodes are the new ones or the ones still awaiting replacement. Those two cases mean opposite things — a rollout nearly finished versus one that has barely started replacing unhealthy nodes — and a consumer restricted to a single resource's status cannot tell them apart.

### Redefining `status.nodes`

The four new fields are counted over the NodePool's NodeClaims. `status.nodes` is the natural denominator for them, and is already the field consumers reach for, but its current definition cannot serve that role. `nodepool.counter` reads it back out of the cluster-state resource accounting:

```go
nodePool.Status.Resources = lo.Assign(BaseResources, c.cluster.NodePoolResourcesFor(nodePool.Name))
nodeQuantity := nodePool.Status.Resources[resources.Node]
nodePool.Status.Nodes = new(nodeQuantity.Value())
```

That accounting omits two groups. NodeClaims marked for deletion contribute nothing (`updateNodePoolResources` substitutes an empty `ResourceList` when `StateNode.MarkedForDeletion()`), and `MarkedForDeletion` is set as soon as the disruption controller *selects* a candidate — long before the instance is gone. NodeClaims that have not launched are also absent, because `Cluster.UpdateNodeClaim` only creates a `StateNode` once `status.providerID` is set.

Both exclusions apply at the same moment, and both understate the denominator. Take a 20-node pool near the end of a rollout, with the default 10% disruption budget: 18 replacements are up and ready, the last 2 outdated nodes have been selected for disruption and are draining, and their 2 replacements have been created but not yet launched. Counted over the 22 NodeClaims that exist, `upToDateAndReadyNodes` is 18 and `upToDateNodes` is 20. `status.nodes` reports 18 — the 2 draining nodes are excluded as marked for deletion, and the 2 unlaunched replacements were never `StateNode`s.

The gate above then evaluates 18 against 18, returns Healthy, and releases the next Argo Application while two outdated nodes are still running workloads and two replacements have yet to come up. On the NodeClaim basis it evaluates 18 against 22 — 82% — and correctly reports Progressing. The same arithmetic also puts `upToDateNodes` (20) above `status.nodes` (18), which is incoherent for a pair of fields consumers are expected to divide.

So `status.nodes` is redefined to count the NodePool's NodeClaims, including ones that have not launched and ones that are terminating, and all five fields are computed from a single NodeClaim list in a single status patch. Terminating outdated nodes staying in the denominator until they are gone is the behavior a rollout gate wants: the pool reports incomplete until the replacement is actually in place.

One consequence to settle: `status.nodes` currently *is* `status.resources["nodes"]`, and decoupling them means the two keys can disagree by the number of unlaunched and terminating NodeClaims. The split is defensible — `status.resources` is a report of schedulable capacity, where excluding a draining node is correct, while `status.nodes` is a replica count — but it should be stated in the field documentation rather than discovered.

### Which drift vectors count

[The `Drifted` condition is the union of several independent causes, and collapsing that union into a NodePool-level number produces a signal that means different things at different times.](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562)

| Drift vector | Detected by | Counts against `upToDateNodes`? |
|---|---|---|
| NodePool `spec.template` static fields | `areStaticFieldsDrifted` (hash compare, core) | **Yes** |
| NodePool `spec.template.spec.requirements` | `areRequirementsDrifted` (label compatibility, core) | **Yes** |
| NodeClass spec change | `cloudProvider.IsDrifted` (e.g. `NodeClassDrifted`) | No (v1) |
| Out-of-band cloud provider change (new AMI, capacity reservation reshuffle, ...) | `cloudProvider.IsDrifted` | No |
| Instance type no longer offered | `instanceTypeNotFound` (core) | No |

The narrow definition makes the signal correct for use cases 1–3. In a fleet with frequent out-of-band drift (AMI releases on a weekly cadence, reservation churn), a gate keyed on the drift union never closes, so a GitOps pipeline sequenced on it stalls forever on changes unrelated to what was pushed. Conversely, use case 4 wants the union, and it is a time-series question that metrics answer better than status does. The split emphasizes that:

- **Status** answers "did the declarative change I applied finish propagating?" — narrow, revision-anchored, level-triggered, consumable by single-resource evaluators.
- **Metrics** answer "what is the state of drift across my fleet right now and over time?" — the full union, sliced by reason. That is #3177's job, extended with a `reason` label ([Observability](#observability)).

### How It Works

A NodeClaim is up to date with respect to a NodePool at generation `G` when, evaluated against the NodePool spec at `G`:

1. `nodeClaim.annotations["karpenter.sh/nodepool-hash-version"]` equals `v1.NodePoolHashVersion` **and** `nodeClaim.annotations["karpenter.sh/nodepool-hash"]` equals `nodePool.Hash()`, and
2. `areRequirementsDrifted(nodePool, nodeClaim)` returns `""`.

Both are the existing NodePool-attributable halves of `Drift.isDrifted`. NodeClaims whose hash version does not match the current one are counted as not up to date; that window is transient (the `nodepool.hash` controller re-stamps them) and erring toward "still rolling out" is the safe direction.

A NodeClaim is ready when its `Ready` status condition is `True`, which the NodeClaim API already defines as the roll-up of `Launched`, `Registered`, and `Initialized`. No new readiness definition is introduced; `readyNodes` is a count of an existing per-NodeClaim signal.

The `nodepool.counter` controller computes the counts, rather than a new controller: it already reconciles every NodePool on a 5s requeue and already owns a status patch, so the marginal cost is one NodeClaim list per pass and the counts land atomically alongside `status.resources` instead of racing a second writer. The change is to list the NodePool's NodeClaims (`nodeclaimutils.ListManaged(ctx, client, cloudProvider, nodeclaimutils.ForNodePool(name))`), bucket them, and write all five fields in the same status patch. Listing NodeClaims rather than walking cluster state is what makes the redefined `status.nodes` include NodeClaims that have not yet launched or registered a Node, and is deliberate for the same reason: those are exactly the replacements a rollout is waiting on. `status.resources` continues to be derived from cluster state, unchanged.

Two details make the result trustworthy:

- **Compute the hash, don't read the annotation.** The counter calls `nodePool.Hash()` on the object it is reconciling rather than reading `nodePool.annotations["karpenter.sh/nodepool-hash"]`. The annotation is written by a separate controller, so reading it opens a window where `metadata.generation` is already `G+1` while the annotation still holds the `G` hash — the counter would then report "everything up to date" and stamp `observedGeneration: G+1`, passing a gate prematurely. Deriving the hash from the spec in hand closes that window by construction.
- **Write `observedGeneration` in the same patch as the counts.** The counts and the generation they were derived from must never be observable independently.

### Linearizability

The other concern in #3108: counts reconciled asynchronously can be arbitrarily stale under CPU starvation or client throttling, so a gate can pass on numbers computed before the change landed. Edge detection ("the drifted count went up") does not fix it, because a NodePool update is not guaranteed to induce drift at all — restricting requirements to prune instance types that were never in use bumps the generation and drifts nothing.

Anchoring the counts to the generation resolves both halves:

| Scenario | `metadata.generation` | Status after counter runs (`upToDateNodes`/`nodes`) | Consumer sees |
|---|---|---|---|
| Spec change that drifts nodes | `G+1` | `observedGeneration: G+1`, `14/20` | Progressing |
| Same, counter starved | `G+1` | stale `observedGeneration: G` | Progressing (generation mismatch) |
| Spec change that drifts nothing (`limits`, pruned unused requirements) | `G+1` | `observedGeneration: G+1`, `20/20` | Healthy immediately — correct, no rollout was needed |
| Rollout completes | `G+1` | `observedGeneration: G+1`, `20/20` | Healthy |

The third row is the case that defeats edge-triggered designs and that a level-triggered, revision-anchored count handles for free: the counter recomputes up-to-dateness every pass, so "nothing needed to change" and "everything already changed" are indistinguishable.

### Interaction with Existing Features

- **Disruption budgets / drift back-off.** Unchanged. Budgets and back-off govern *how fast* `upToDateNodes` converges; they do not change what is counted. A pool that is backed off or budget-blocked simply reports incomplete for longer.
- **Terminating NodeClaims.** A NodeClaim with a deletion timestamp still counts in `nodes` until it is gone — a change from today's behavior, per [Redefining `status.nodes`](#redefining-statusnodes). If it is outdated, the pool keeps reporting incomplete until the replacement is in place, which is what a rollout gate wants.
- **Static NodePools (`spec.replicas`) and the scale subresource.** Same accounting applies; no special casing. The redefinition brings `status.nodes` onto the same basis the static provisioning and deprovisioning controllers already use to satisfy `spec.replicas`, so a pool at its replica count now reports `nodes == spec.replicas` mid-rollout instead of dipping below it.
- **`do-not-disrupt` NodeClaims.** These can pin `upToDateNodes` below `nodes` indefinitely. This is a correct report, and the reason the API exposes counts rather than a boolean: consumers set a tolerance.
- **Hash version bumps across Karpenter upgrades.** Existing behavior already re-stamps NodeClaims that are not drifted; during the window the counts read conservatively low.
- **NodeClaims with no `nodepool-hash` annotation** (e.g. adopted/hydrated from an older version): counted as not up to date, consistent with the conservative direction. Worth confirming against the hydration controller's behavior before implementation.

### Observability

Status is only half the answer. The complementary metric work in #3177 only needs one addition: label the drift condition with its reason, which the NodeClaim condition already carries (`SetTrueWithReason(ConditionTypeDrifted, driftedReason, ...)`).

```
karpenter_nodepools_nodeclaim_condition{nodepool, condition, status, reason}
```

That single label is what gives use case 4 the differentiated view  — `reason="NodePoolDrifted"` vs `reason="AMIDrift"` vs `reason="NodeClassDrifted"` — without putting the taxonomy in the API. Reason values are a bounded, provider-defined set, so cardinality is manageable.

Additionally we could also add:

- `karpenter_nodepools_nodes{nodepool}`, `karpenter_nodepools_uptodate_nodes{nodepool}`, `karpenter_nodepools_ready_nodes{nodepool}`, and `karpenter_nodepools_uptodate_and_ready_nodes{nodepool}` gauges mirroring the status fields, so the same gate can be alerted on ("pool has been < 90% up to date and ready for 2h").
- An event on the NodePool when `NodesUpToDate` transitions, so `kubectl describe nodepool` shows rollout start/finish.
- Optional printer column `ROLLOUT  12/21`, from `upToDateAndReadyNodes` over `nodes`, alongside the existing `Nodes` column.

### Edge Cases

- **NodeClaim created from the previous revision, not yet in the informer cache.** A NodeClaim launched from spec `G` concurrently with the update to `G+1` can be briefly invisible, letting the pool report `20/20` before flipping back to `20/21`. The window is bounded by watch latency, and the consequence is a transient Healthy → Progressing flap rather than a stuck-Healthy. All other cache-lag directions are conservative: an unobserved new NodeClaim is by definition up to date, a stale cached entry for a deleted outdated NodeClaim only makes the pool look less complete.
- **Empty NodePool.** All four counts are `0` and the condition is `True`. Consumers that need "nonempty and settled" check `nodes > 0` themselves.
- **Unready nodes that are not part of a rollout.** A NodeClaim stuck launching for unrelated reasons holds `readyNodes` below `nodes` with no rollout in flight. This is the same shape as a Deployment with a crash-looping pod: the count is accurate and the consumer's tolerance decides whether it blocks. The existing `NodeRegistrationHealthy` condition remains the signal for a NodePool that cannot launch nodes at all.
- **NodePool with `Ready: False`** (bad NodeClass reference): counts still reported; the existing `Ready` condition is the signal for that failure mode.
- **Rapid successive edits.** Each bumps the generation; the gate stays open until the counter observes the latest one.

## Alternatives Considered

**Aggregate the existing `Drifted` condition onto the NodePool.** Directly what the issue requested and what #3108 implemented. Rejected as the primary mechanism for the two reasons above: the union semantics make the number mean different things in different clusters, and the condition is written asynchronously with a 5-minute requeue and no revision anchor, so it cannot support a correct gate. The revision comparison is strictly more precise for this use case and strictly cheaper to compute.

**A NodePool-level `Drifted` condition only.** Simple boolean gate, but forces all-or-nothing semantics — no tolerance for a single `do-not-disrupt` node — and carries the same union ambiguity. The proposed `NodesUpToDate` condition provides the boolean for consumers that want it, defined against the revision instead of the union.

**Metrics only (#3177).** Argo CD health checks and kro `readyWhen` cannot read Prometheus; the evaluation sandbox sees one resource. Metrics are complementary, not a substitute — hence both.

**ControllerRevision-based accounting, like DaemonSet.** Materializing revisions would give richer history (which revision each NodeClaim belongs to, rollback support) but introduces a new persisted object per revision and a garbage collection story, for information the existing hash annotation already encodes.

### Extending beyond NodePool-attributable drift

Several reporters will eventually want the gate to cover NodeClass changes too — Argo syncs the NodePool and the NodeClass in the same Application, so "my change finished rolling out" arguably spans both. Core cannot compute that today: NodeClass up-to-dateness is provider-specific (the AWS provider stamps its own `karpenter.k8s.aws/ec2nodeclass-hash` on NodeClaims) and reaches core only as an opaque `DriftReason` string.

The clean extension is to categorize drift reasons at the cloud provider boundary — for example having `IsDrifted` return a reason plus a category (`StaticNodeClass` vs `Dynamic`) — which would let core fold static NodeClass drift into `upToDateNodes` and, separately, improve the metric labeling. That is a cloud-provider interface change affecting every provider, so it is proposed as follow-up work rather than a prerequisite. `status.nodeClassObservedGeneration` already exists and gives consumers a partial NodeClass-side signal in the meantime.

## Backward Compatibility

The four new fields and the condition are additive and read-only, and no YAML needs to change. Users must apply the updated CRDs to see the fields, per the usual Karpenter CRD upgrade path. `NodesUpToDate` is not part of the `Ready` aggregate, so `Ready` semantics are unchanged for existing consumers.

`status.nodes` is the one field whose meaning changes. It is read-only, so nothing breaks structurally, but its value shifts: it now includes NodeClaims that have not launched and NodeClaims that are terminating, so it reads higher than before during provisioning and disruption and is unchanged for a steady-state pool.

## Graduation Criteria

No feature gate proposed. The change is only additive, read-only, computed from data Karpenter already maintains, and has no effect on provisioning or disruption behavior. The main risk is API shape, which is what this RFC is for.

## Open Questions

1. **Is `observedGeneration` alone sufficient, or should the status also echo the `nodepool-hash`?** The generation is the conventional anchor and is what `kstatus` checks, but the hash is what up-to-dateness is actually computed against. Echoing both is cheap and would let consumers distinguish "spec changed but not in a drift-relevant way."
2. **Should `status.nodes` and `status.resources["nodes"]` be reconciled rather than allowed to diverge?** The proposal keeps `status.resources` on cluster state and moves only `status.nodes`. Keeping both on the NodeClaim basis would be more internally consistent but would make `status.resources` report the capacity of nodes that are draining or have not launched.

## References

- Issue: [Surface NodeClaim drift/rollout progress in NodePool status (#3071)](https://github.com/kubernetes-sigs/karpenter/issues/3071)
- Prior implementation attempts: [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) (status), [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177) (metrics)
- Maintainer feedback this RFC responds to: [#3071 (comment)](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562)
- Drift semantics: [`designs/drift.md`](./drift.md), [`designs/drift-hash-versioning.md`](./drift-hash-versioning.md)
- Precedent: Deployment `status.updatedReplicas`/`observedGeneration`; Cluster API `MachineDeployment.status.upToDateReplicas` and the Machine `UpToDate` condition; [`kstatus`](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus)
