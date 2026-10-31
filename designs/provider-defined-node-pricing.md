# Provider-defined node pricing

Draft — 2026-10-27. Interfaces and rollout remain proposals. Sources verified on October 24; no implementation, benchmarks, or live cloud validation.

## Motivation

Karpenter derives node prices from shared offerings keyed by instance type, zone, and capacity type, backed by provider pricing caches and adjusted by NodeOverlays. Sharing prices reduces state, memory, and reconciliation work without publishing a price update for every node.

However, nodes with the same offering attributes can have different modeled costs as provider-observed state changes. Offering-level prices cannot express those differences without changing the attributes used for lookup. This limits runtime reservation matching and provider models informed by reservation-wide usage. Consolidation needs these changing inputs while retaining efficient shared pricing.

This RFC proposes an optional way for providers to define shared pricing modifications and identify the nodes they apply to. Core applies NodeOverlays, shares effective values, and updates consolidation and accounting when a modification or its membership changes. Providers that do not need this capability retain ordinary shared offering pricing.

## Current architecture and scope

AWS combines base, reserved, placement-group, and extension logic into offerings. Core applies precomputed NodeOverlay policy through selective copy-on-write, normally leaving provider caches unchanged. Both AWS and Azure also apply overlays during actual launch. ([Provider offering construction](#provider-offering-construction); [Existing overlay machinery](#existing-overlay-machinery); [Azure launch integration](#azure-launch-integration))

`NodeClaimPriceProvider` lets providers supply prices for running nodes, but does not define how those prices combine with NodeOverlays or how changes reach consolidation and cost accounting. Current cost tracking also treats these custom prices as fixed. The existing hook may remain for migration compatibility. ([Existing running-price hook](#existing-running-price-hook); [Cost update gap](#cost-update-gap))

Pricing already changes through provider/overlay refreshes. AWS patches reserved-to-On-Demand demotions, enabling subsequent offering lookup to reflect expiration. Azure uses a large price penalty for missing pricing and unfamiliar SKUs, while its constructed offering cache does not track price-table revisions. These paths show why knownness, preference, and update propagation need explicit semantics. ([Provider-owned observation](#provider-owned-observation); [Azure pricing and cache dependencies](#azure-pricing-and-cache-dependencies))

## Proposal

Introduce an optional provider pricing catalogue: shared pricing modifications plus their applicability to nodes. Providers define the valuation rules and determine which nodes qualify from observed state. Nodes can join, leave, or switch pricing groups without changing their scheduling labels. Core helpers manage membership bookkeeping, cached values, and dependent updates.

A catalogue entry describes a provider valuation rule or value before user overlays; it need not be one fixed price. Effective values are shared only where base inputs, provider rules, and overlay context are equivalent. The catalogue is an internal integration, not a proposed user-managed Kubernetes resource.

Consider two nodes of the same instance type in the same zone and NodePool. Node A is confirmed uncovered and uses the ordinary $0.10/hour offering price. Node B is covered and belongs to a provider-defined reservation pricing group, which values it at $0.01/hour before user overlays.

| Node | Provider pricing membership | Before user overlays |
| --- | --- | --- |
| A | Confirmed no modification; ordinary shared price | $0.10/hour |
| B | Reservation pricing group | $0.01/hour |

When A gains coverage, it joins the reservation pricing group; when B loses coverage, it returns to ordinary pricing. Core updates their effective values and accounting contributions. Changing the group's valuation or a shared overlay updates the affected cached values without calculated-price patches to every NodeClaim. An uninitialized or failed observation is unknown, not evidence that no modification applies.

This example assumes observable individual coverage. Azure may instead model a set of nodes from reservation utilization; group membership must not imply individual coverage that the provider cannot establish. Exact group savings remain subject to the [limits of additive valuation](#limits-of-additive-valuation).

### How scheduling and consolidation use prices

| Consumer | Pricing inputs |
| --- | --- |
| Scheduling and launch | Offering prices for ordering eligible new-capacity options, with user overlays |
| Consolidation | Effective prices of existing nodes, including applicable provider modifications, compared with replacement estimates |
| Accounting | The same effective existing-node prices used by consolidation |

Launch and running nodes are consumers of shared pricing machinery. Existing-node membership does not transfer to a replacement. A reservation-only request can use a covered value conditional on success; a request allowing uncovered fallback must account for that outcome. A launch preference can rank an option without establishing its cost.

Unknown cost remains distinct from zero or a sorting penalty. A node's modeled value is also not necessarily its deletion savings when reservation capacity can transfer between nodes.

### Provider integration

The optional integration needs three capabilities; exact Go APIs remain open:

```text
Pricing definitions + revisions  -> shared provider valuation rules/values
Classify(NodeClaims)             -> group membership / no modification / unknown
Notify(changed scopes)           -> refresh affected memberships and values
```

Providers supply observations, definitions, and applicability; core helpers maintain the catalogue and membership lifecycle. Batched classification and valuation read a consistent initialized local view. No cloud calls occur from pricing lookups, pod loops, or comparisons. Cloud discovery and actual allocation remain provider-owned. Inject the integration with the provider rather than through global registration.

| Provider needs | Integration |
| --- | --- |
| Ordinary shared pricing | Existing offering path; no catalogue integration required |
| Selected-node modifications | Shared definitions and batched applicability from observations; explicit no-modification and unknown results |
| Fixed instance price | Recorded provider value; distinguish pre-overlay from legacy already-effective values |
| Group-dependent economics | Provider-defined runtime values initially; separate action evaluation before claiming exact group savings |

A participating provider explicitly confirms no modification; a missing result or failed lookup never implies fallback. Do not collapse entries with different economic/policy dependencies merely because type, zone, and capacity type match. Without safe sharing dependencies, core uses bounded individual evaluation. Whether multiple modifications may apply, and how they compose, remains open.

Legacy adapters cannot recover knownness or fallback semantics from `Offering.Price` alone. Providers must expose that information and connect price-cache invalidation before opting into the new guarantees; legacy compatibility does not imply full conformance.

Shared helpers should provide snapshot replacement, revisions, and conservative scope-level notifications. Commit state before notifying, reject stale work, and support startup/resync without event replay. Snapshots may share immutable structures; existing mutable maps require short-lock capture, copying per update, or immutable replacement. Exact APIs and scope boundaries remain open.

### Core evaluation and updates

Apply the selected NodeOverlay exactly once after provider valuation. Preserve precedence/conflicts, absolute overrides, relative adjustments, and nonnegative clamping. Absolute overlays can supply missing base values; relative adjustments require a base. An overlay cannot create availability or reservation slots. Preserve capacity overlays separately. Costs must be finite, nonnegative, and in consistent modeled units/hour, preserving existing precision. ([Existing overlay machinery](#existing-overlay-machinery))

Cache compiled matches separately from numeric results, using complete dependencies: provider model, NodePool context, instance/placement attributes, stable capacity type, and applicable reservation/custom labels. Match on relevant status/identity changes as well as spec changes. Runtime evaluation must work independently of launchable offerings; current NodePool versus historical/NodeClaim matching context remains open.

| Change | Affected work |
| --- | --- |
| Free reservation slots | Launch feasibility; do not unnecessarily reprice covered nodes |
| One node's membership | Effective value, aggregate contribution, affected decisions |
| Shared price/model | Dependent effective values and aggregates; no capability-cache TTL dependency |
| Overlay or matching context | Recompile affected old/new matches, including running-only contexts |
| Node deletion | Idempotent membership removal and bounded cache reclamation |

Providers decide membership; core/helpers store and index it alongside effective-value caches. Share by semantic dependencies, not numeric equality. Preserve `Offering.Price` through prepared views/selective copy-on-write for launch compatibility, retaining knownness separately. Node-specific modifications do not mutate those shared launch offerings. Reuse the overlay evaluator for existing-node values, simulation, and provider launch. Preserve AWS's missing-price overlay bootstrap during migration. ([Provider offering construction](#provider-offering-construction))

### Scheduling and launch outcomes

1. `Provisioner.NewScheduler()` obtains eligible NodePool offerings through `GetInstanceTypes()`. Capture initialized model/policy views and prepare launch values.
2. `Scheduler.Solve()` retains feasibility, NodePool weights, and bin packing. Comparisons read prepared values without repeated overlay scans.
3. `NodeClaimTemplate.ToNodeClaim()` selects options through `InstanceTypes.OrderByPrice()`. Consolidation estimates costs across outcomes the replacement request permits, rather than assuming the cheapest option launches.
4. Lifecycle calls provider `Create()`. AWS Fleet and Azure's client-side allocation retain their strategies and use shared policy preparation. Execution may use newer observations but must preserve approved constraints across retries.

([Scheduling and launch integration](#scheduling-and-launch-integration); [Azure launch integration](#azure-launch-integration))

| Reservation launch mode | Economic assumption |
| --- | --- |
| AWS explicitly targeted, reservation-only | Covered valuation conditional on success; insufficient reserved capacity fails the attempt |
| Proposed AWS group mode with reserved-first fallback | Both covered and uncovered outcomes are permitted; a conservative estimate includes uncovered pricing |
| Azure CRG association | Association does not establish individual coverage; overallocation is permitted |

An observed free slot is not a guaranteed admission. Use existing requirement constraints where sufficient; do not add a quote-token protocol without a demonstrated need. Expected-price policies require an explicit model and are outside this proposal. ([Reservation launch semantics](#reservation-launch-semantics))

### Runtime consumers and recovery

Use consistent runtime values for disruption ordering/source valuation, same-instance-type checks, balanced totals, cluster/NodePool accounting and metrics, and pod deletion-cost ranking. Replacement estimates use the launch model. For additive accounting:

```text
NodePool total = sum(member count × effective shared value) + individual exceptions
```

Track unknown contributions and completeness, including noncandidate nodes in balanced-scoring denominators. Membership changes, deletions, relists, duplicate events, and migration must be idempotent. Coalesce invalidation and recheck economics before accepting a price-dependent command; rebuild or discard changed plans. Current accounting and validation need changes. ([Running-price consumers](#running-price-consumers); [Cost update gap](#cost-update-gap); [Consolidation gaps](#consolidation-gaps))

Failed/incomplete observation retains known facts; omission never establishes lost coverage. Retain the last usable view on transient evaluation failure, without publishing unoverlaid intermediates. Distinguish unavailable policy from evaluated empty policy; removal recomputes from base. Local consistency does not imply simultaneous cloud observations or guaranteed freshness; age alone does not invalidate a known value.

An adapter can retain a disappeared offering's last value in memory, but restart recovery requires reconstructible inputs or persistence. Durable coverage alone does not preserve the exact last effective price. Defer affected economic decisions while required inputs initialize; readiness and unrelated provisioning continue. Existing disruption lifecycle, budgets, PDBs, and feasibility remain; no post-commit rollback is promised.

### AWS and Azure adoption

**Existing AWS ODCR:** reuse price tables, reserved valuation (On-Demand / 10,000,000), reservation discovery/targeting, slot accounting, and instance observations. Keep reservation IDs and quantities on offerings; runtime valuation becomes independent of free slots. Existing label/demotion behavior need not change. Group gain/loss/reassociation requires extending current observation/classification. ([Current reservation valuation](#current-reservation-valuation); [Fleet listing and parsing](#fleet-listing-and-parsing))

**Azure:** retain CRG SKU/placement constraints, `on-demand` labels, and VM/AKS Machine allocation. Existing pricing can use the default adapter. Automatic utilization-based valuation requires a new provider-owned observer: current discovery records member quantity and placement, not allocated consumers. Azure exposes membership and allocated VM IDs but no covered/excess subset under overallocation. External consumers and stopped-but-allocated VMs matter; deallocated VMs do not. Different SKU/placement members are not fungible. ([Azure reservation implementation](#azure-reservation-implementation); [Azure API and lifecycle](#azure-api-and-lifecycle))

Azure maintainer `tallaxes` welcomed comparing approaches and highlighted the poor NodePool-per-SKU experience. This motivates cross-provider support, not agreement on this interface. Shared pricing alone does not remove the current per-member NodePool workaround. ([Azure reservation guidance](#azure-reservation-guidance))

### Limits of additive valuation

For one member with fixed successfully reserved quantity `C`, allocated quantity `A`, and unit rate `p`, excluding discounts and ancillary costs:

```text
F(A) = p × max(C, A)
C=1, A=2: deleting either VM saves p; deleting both still saves only p.
```

No fixed per-node allocation represents all three savings. This also affects AWS group matching: deleting the covered instance can let an uncovered instance inherit its unit. Observable coverage alone does not solve action economics.

This RFC supports provider-defined runtime values under an additive modeled objective. Exact group savings need separate evaluation of the proposed transition, including retained nodes, permitted replacements, external consumers, and ordering/overlap effects. Unsupported group evaluation must not silently fall back to additive savings. Reservation-release controllers can also change the useful model. ([Group RFC discussion](https://github.com/aws/karpenter-provider-aws/pull/9713#discussion_r4342068983))

NodeOverlay composition with that extension remains open: adjusting a group delta is not generally equivalent to applying per-node policy, especially absolute overrides. Preserve existing additive overlay semantics initially; do not claim general group optimization. Reserved VM Instances/Savings Plans discounts remain distinct from capacity guarantees.

## Performance and observability

Target no significant regression for shared pricing, including fleets where most nodes use runtime adjustments. Core/helpers should maintain shared pricing contexts and node membership. Cost depends on distinct contexts and change frequency: 10,000 nodes sharing 200 contexts can reuse 200 effective values while retaining O(N) membership. Unique inputs eliminate that reuse; performance remains unmeasured.

Separate classifying nodes from calculating prices. A full inventory refresh may inspect O(N) nodes, but a scan with no input changes must not trigger valuation, overlay matching, or aggregate updates. Scheduling reuses prepared values without pricing cloud calls, per-node repricing, inner-loop rule scans, or capability copies solely for price reads. Recompute affected contexts, coalesce updates, and bound retained revisions; group changes may legitimately affect many nodes.

Reuse provider observation caches; changed durable facts still require individual patches. AWS bulk instance listing and its separate lookup batcher already support shared observation. Azure fetches price tables before taking its update lock; AWS currently holds price locks during fetching. These are implementation precedents, not performance guarantees. ([Fleet listing and parsing](#fleet-listing-and-parsing); [Separate individual-lookup batching](#separate-individual-lookup-batching); [Provider price tables and currency](#provider-price-tables-and-currency); [Azure pricing and cache dependencies](#azure-pricing-and-cache-dependencies))

Benchmark against current upstream: unchanged scheduling, majority/all-runtime-adjusted fleets, broad price/policy changes, sparse/mass runtime updates, many pools/policies, unique contexts, and cold starts/failures. Measure throughput, p95/p99 latency, CPU, allocations, retained memory, lock waits, API/watch traffic, aggregate work, and convergence. Set numerical acceptance budgets from baselines before broad enablement.

Expose bounded-cardinality initialization/unknown, failure, cache-size, and convergence metrics. Diagnostics should explain the model, selected overlay, effective value, and launch assumptions; keep object IDs and raw prices out of metric dimensions.

## Alternatives

| Approach | Benefit | Cost / limit |
| --- | --- | --- |
| Optional shared modifications + core helpers (proposed) | Provider-owned rules/applicability; shared evaluation and centralized updates | Dependency tracking and membership integration still require work |
| Provider-managed catalog storage and bindings | Explicit identities and incremental publication | Duplicates lifecycle, indexing, and reclamation work across providers |
| Extend offerings only | Smaller migration | Runtime valuation stays coupled to launch identities/outcomes |
| Final effective price in NodeClaim status | Inspectable, durable last value | Broad patch fan-out; still needs consistent aggregates/invalidation |
| Per-node scalar callback | Small reader API | Hidden dependencies, policy ownership, and updates; batching/reuse unspecified |

Persist final values if exact last-price continuity is required and inputs cannot be reconstructed. Push versus pull alone does not resolve the semantic gaps; either needs explicit dependencies, knownness, and updates.

## Rollout and validation

Providers pin core in one binary and distribute matching CRDs. Coordinate releases; feature gates do not make breaking Go interfaces source-compatible. ([Provider release integration](#provider-release-integration))

1. Define knownness, policy ownership, permitted outcomes, and update semantics. Supply legacy/default adapters and fix mutable accounting/invalidation/revalidation.
2. Share policy preparation across scheduling, runtime consumers, and actual launch; connect price and matching-context revisions.
3. Opt providers into shared modifications and membership updates. Persist required observed facts; install matching CRDs first if their schemas change. Reject unsupported enablement. Legacy already-effective values must not receive overlays again.
4. Gate affected consolidation on usable source, replacement, and aggregate inputs, including noncandidate denominators. Provisioning needs launch initialization, not every runtime value. Define disablement/downgrade behavior for nodes legacy readers cannot price correctly.

Providers/clusters can adopt independently. Broad enablement requires these conformance cases plus measured performance; they are requirements, not completed tests:

| Case | Required result |
| --- | --- |
| Slot consumed after planning | Strict launch fails; fallback estimates include uncovered outcome |
| Covered node, zero free slots | Runtime value remains usable independently of launch feasibility |
| Unknown cost with ranking penalty | Launch policy may allow it; accounting/savings remain unknown |
| Price or status-only matching change | Refresh dependent values/matches without capability TTL or per-node price writes |
| Missing offering, failed observation, restart | Retain/reconstruct known inputs or report unknown; no fabricated zero/uncovered |
| Overlay create/edit/remove/conflict | Correct precedence, missing-base absolute override, no compounding; preserve capacity overlays |
| Runtime update/noncandidate change | Mutable contributions, complete denominators, economic revalidation |
| Migration and scale | Simple/shared/fixed-value providers, mixed modes, retries/deletion/relist, rollback, bounded memory/convergence |

## Open questions

1. Definition/classification APIs, revision scopes, change delivery, retention, and helper ownership; whether multiple modifications compose.
2. Launch-outcome representation and enforcement across provider strategies/retries.
3. Durable fact schemas, fixed-price policy semantics, and exact-last-value requirements.
4. Runtime overlay context after NodePool/offering changes; safe sharing keys and unknown aggregates.
5. Group action-cost/overlay composition and unsupported-model behavior.
6. Release stages, commitment-boundary integration, rollback, and measured performance thresholds.

## Current upstream evidence

Verified on 2026-10-24 against upstream `main`: core `fac245b5e8980b79fcc28bb65b136ab0c32bb0d8` and AWS `c27c59990c67b1ba2324ce32e36e30eac637b4ae`. These are source snapshots from upstream main, not the reservation-group RFC or implementation-prototype branches.

Azure implementation and guidance were checked at `298f6f998fa77bb7e64d51e78913ff8f33f8b857`, including its pinned core `v1.14.1`. AWS pins core `fff8e4dbef9d`; core main is a separate, newer snapshot. These are source audits, not a tested release combination.

### Existing running-price hook

[`NodeClaimPriceProvider`, registration, default offering lookup, and validation](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/cloudprovider/types.go#L875). It returns hourly price plus known/unknown and rejects negative/nonfinite results. [`NodeClaimStatus`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/apis/v1/nodeclaim_status.go#L55) has no modeled-price field.

The hook was introduced by [core PR #3457](https://github.com/kubernetes-sigs/karpenter/pull/3457), commit `fff8e4dbef9dea9834838fbe077a3afd65066041`, on October 21. Before it, running candidates used `ResolveOfferingPrice`. No AWS override was found in the inspected snapshot.

### Provider offering construction

[AWS resolver chain](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/instancetype/offering/offering.go#L110) combines base, reserved, placement-group, and extension logic. [Missing-price overlay bootstrap](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/instancetype/offering/offering.go#L195) accepts validated absolute-price overlays with exactly one instance-type `In` requirement; it is separate from core's general evaluator.

### Scheduling and launch integration

[`Provisioner.NewScheduler`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/provisioning/provisioner.go#L282), [`NodeClaimTemplate.ToNodeClaim`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/provisioning/scheduling/nodeclaimtemplate.go#L109), and [lifecycle provider creation](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/nodeclaim/lifecycle/launch.go#L93) define the current call path. [AWS `Create`](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/cloudprovider/cloudprovider.go#L135) reapplies the overlay view to its fresh catalog; [Fleet priorities](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/instance/instance.go#L610) use offering prices when overlays are enabled.

### Reservation launch semantics

[Core launch ordering](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/cloudprovider/types.go#L396) and [`WorstLaunchPrice`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/cloudprovider/types.go#L677) serve different purposes. [Replacement filtering](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/provisioning/scheduling/nodeclaim.go#L457) considers launchability. [AWS targeted templates](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/launchtemplate/types.go#L151) use `capacity-reservations-only`; [Fleet documentation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-fleet-on-demand-capacity-reservations.html) distinguishes strict targeting from reserved-first fallback. [Group lifecycle](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/cr-groups-lifecycle.html) documents subsequent reassociation.

### Azure launch integration

[Launch resolution](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/cloudprovider/cloudprovider.go#L606) applies overlays before [client-side allocation](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/providers/allocationstrategy/allocation_strategy.go#L65). [Ranking](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/providers/allocationstrategy/stages/offering_rank.go#L76) compares price, capacity type, and placement; optional recommendations filter candidates.

### Azure pricing and cache dependencies

[Pricing](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/providers/pricing/pricing.go#L35) uses a `999` missing-price penalty and fetches before locking. [Offering construction](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/providers/instancetype/instancetypes.go#L393) discards knownness and penalizes unfamiliar SKUs. Its [23-hour cache](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/providers/instancetype/instancetypes.go#L56) excludes utilization from construction keys; [source generations](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/providers/instancetype/instancetypes.go#L288) track quota/availability, not pricing. No propagation delay was measured.

### Azure reservation implementation

[Group reconciliation](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/controllers/nodeclass/status/capacityreservationgroup.go#L117) records [member quantity/state/placement](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/controllers/nodeclass/status/capacityreservationgroup.go#L294), without utilization instance view. [Offering projection](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/pkg/providers/instancetype/instancetypes.go#L333) restricts SKU/placement while keeping `on-demand` pricing. [Azure's core pin](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/go.mod#L62) predates the running-price hook; its [runtime lookup](https://github.com/kubernetes-sigs/karpenter/blob/v1.14.1/pkg/controllers/disruption/types.go#L112) returns zero for missing offerings.

### Existing watch and offering refresh

[`state.nodeclaim`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/state/informer/nodeclaim.go#L60) updates cluster and cost state. [`state.pricing`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/state/informer/pricing.go#L55) polls `GetInstanceTypes` per NodePool and requeues after one hour on success; it does not observe running instances through `List`.

### Cost update gap

[`ClusterCost`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/state/cost/cost.go#L166) returns immediately for already tracked NodeClaims. Its fixed-price contributions survive offering refreshes but have no mutable update path. [`NodePool metrics`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/metrics/nodepool/controller.go#L121) read that aggregate.

### Existing overlay machinery

[`GetInstanceTypes` decorator](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/cloudprovider/overlay/cloudprovider.go#L49), [controller evaluation and watches](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/nodeoverlay/controller.go#L72), [evaluated store](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/nodeoverlay/store.go#L43), and [price API](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/apis/v1alpha1/nodeoverlay.go#L77). The controller watches overlays, NodePools, and supported NodeClasses, replaces the store, and marks cluster state unconsolidated.

### Running-price consumers

[candidate construction](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/disruption/types.go#L214), [replacement filtering](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/disruption/consolidation.go#L197), [same-instance-type checks](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/disruption/multinodeconsolidation.go#L207), [balanced totals](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/disruption/balanced.go#L48), and [pod deletion-cost ranking](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/pod/deletioncost/ranking.go#L257).

### Consolidation gaps

[`triggerConsolidationOnChange`](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/state/cluster.go#L1018) checks creation/removal, initialization, and deletion state, not price. [Command validation](https://github.com/kubernetes-sigs/karpenter/blob/fac245b5e8980b79fcc28bb65b136ab0c32bb0d8/pkg/controllers/disruption/validation.go#L296) explicitly performs scheduling without price filtering and checks instance-type subsets.

### Provider-owned observation

[AWS capacity-type controller](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/controllers/capacityreservation/capacitytype/controller.go#L66) calls `List`, matches provider IDs, and currently handles reserved-to-On-Demand demotion with individual patches.

### Fleet listing and parsing

[Bulk paginated instance listing](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/instance/instance.go#L238), [cloud-provider mapping](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/cloudprovider/cloudprovider.go#L195), and [reserved classification](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/instance/types.go#L113).

### Separate individual-lookup batching

[`DescribeInstancesBatcher`](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/batcher/describeinstances.go#L40).

### Provider price tables and currency

[Cached On-Demand lookup](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/pricing/pricing.go#L158) and [USD/CNY selection](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/pricing/pricing.go#L361).

[Provider refresh](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/controllers/providers/pricing/controller.go#L43) requeues after 12 hours on success. [On-Demand refresh locking](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/pricing/pricing.go#L185) holds the write lock during fetching; performance improvements discussed above are unmeasured.

### Current reservation valuation

[Reserved offering resolver](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/providers/instancetype/offering/reserved_capacity_resolver.go#L77). [Reservation discovery](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/pkg/controllers/nodeclass/capacityreservation.go#L53) is separate from per-instance association observation.

### Azure reservation guidance

[Capacity reservations](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/docs/capacity-reservations.md) documents the `on-demand` label and unavailable individual SLA-backed-unit attribution; [per-member NodePools](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/docs/capacity-reservations.md#one-nodepool-per-member-reservation), [declared overlay pricing](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/docs/capacity-reservations.md#flexible-a-nodeoverlay), and [overallocation costs](https://github.com/Azure/karpenter-provider-azure/blob/298f6f998fa77bb7e64d51e78913ff8f33f8b857/docs/capacity-reservations.md#costs) establish the operational motivation and limits of those strategies.

### Azure API and lifecycle

Microsoft documents [capacity reservation lifecycle](https://learn.microsoft.com/en-us/azure/virtual-machines/capacity-reservation-overview#capacity-reservation-lifecycle), [pricing and billing](https://learn.microsoft.com/en-us/azure/virtual-machines/capacity-reservation-overview#pricing-and-billing), [overallocation](https://learn.microsoft.com/en-us/azure/virtual-machines/capacity-reservation-overallocate), and the [reservation utilization API](https://learn.microsoft.com/en-us/rest/api/compute/capacity-reservations/get#capacityreservationutilization). The documented allocated-VM list does not identify a covered/excess subset under overallocation.

### Provider release integration

AWS pins core in [`go.mod`](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/go.mod#L54) and registers both sets of controllers in [`main()`](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/cmd/controller/main.go). Its [feature-gate history](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/website/content/en/docs/reference/settings.md#feature-gates) records alpha/beta adoption, and [CRD upgrade guidance](https://github.com/aws/karpenter-provider-aws/blob/c27c59990c67b1ba2324ce32e36e30eac637b4ae/website/content/en/docs/upgrading/upgrade-guide.md#crd-upgrades) requires matching controller/CRD versions. The inspected AWS main pins core `fff8e4dbef9d`; the core-main audit above uses a separate, newer snapshot.
