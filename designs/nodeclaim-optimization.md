# NodeClaim Optimization

## Problem

Karpenter's scheduler can commit a batch of pods to a single NodeClaim whose instance type is much larger — and much more expensive — than most of those pods actually need. The cause is single-NodeClaim packing: `Solve` processes pods in sort order, grows each new NodeClaim's instance-type set as pods are added, and never revisits a placement within the same call. As pods accumulate, the cheapest fitting instance type steps up through tiers; once it steps up, the claim stays at that larger instance type for the rest of `Solve`, regardless of whether an earlier subset would have packed cleanly on the previous tier. A single NodeClaim often ends up holding a set of pods that would be cheaper split across two claims of different tiers.

For example, 16.5 vCPU of pods land on a single 32 vCPU instance at ~51% efficiency. The same pods split as 16 vCPU on a 16 vCPU instance and 0.5 vCPU on a 2 vCPU instance, assuming this is the smallest vCPU sized instance available, run at ~91% efficiency on cheaper total capacity. The scheduler had enough information to choose the second layout; it simply never reconsidered once the 32 vCPU tier was locked in.

## Proposal

Add an **opt-in post-scheduling optimization pass** that, at the end of the scheudling pass, inspects each newly-built NodeClaim efficiency, if a split is cheaper overall, rolls the NodeClaim back to an earlier cheaper state and re-queues the displaced pods. The displaced pods then flow through normal scheduling onto a fresh, right-sized NodeClaim.

The entire feature lives in `pkg/controllers/provisioning/scheduling/` and touches only the scheduler's in-memory simulation — no CRD changes, no persisted state, no user-facing API. The only Go API change is additive and source-compatible: `Topology.Record` now returns a `[]TopologyDelta` (existing callers can silently discard it) and a new `Topology.Unrecord` method replays those deltas. This is discussed in **Topology deltas instead of re-derivation** below.

### Call shape

`Solve`'s outer loop repeats as long as pods remain. When the queue empties, the gated new step runs:

```
Solve loop:
  pop pod → place on existing node / buffered NodeClaim / fresh NodeClaim from template
  ... until queue empty ...
  if s.OptimizationEnabled && tryOptimize(q):    // NEW, gated
      for each unlocked new NodeClaim:
        idx := findSplitPoint()
        if idx >= 0 { displaced += nc.RevertTo(idx) }
        nc.locked = true
      if displaced: *q = NewQueue(displaced)
      continue outer loop                        // re-enter with refilled queue
  break                                          // pass disabled or no displacement
```

Each NodeClaim is visited at most once per `Solve`: once inspected it is *locked* and `CanAdd` rejects further pods. The pass itself runs at most `maxOptimizationPasses` (currently 2) times per `Solve`.

### Transition history

`NodeClaim.Add` already computes the merged requirements and resource requests for each pod it accepts. With the feature enabled, each Add additionally records a `SchedulingOption` snapshot whenever the *cheapest compatible instance type* changes:

```go
type SchedulingOption struct {
    PodCount         int
    InstanceTypes    []*cloudprovider.InstanceType
    Requirements     scheduling.Requirements
    ResourceRequests corev1.ResourceList
    CheapestInstance *cloudprovider.InstanceType
    Price            float64
}
```

A SchedulingOption is captured when the addition of a pod pushes the NodeClaim to a larger sized instance, it is not captured at every pod addition. This is enough information to answer "what would this NodeClaim look like if we stopped accepting pods at point N?"

### Split-point detection

`findSplitPoint` walks the history backwards from the current state looking for the first earlier state that would be worth splitting to. A split is only accepted when four gates all pass:

1. **Price** — the candidate instance type must be at least `minPriceSavingsRatio` (24%) cheaper than current. With cloud instances, pricing is generally by powers of 2, and this decision pushes us back far enough to look back "one size down" in instance families.
2. **Efficiency** — the candidate must be at least `minEfficiencyGain` (10%) more weighted-utilized than the current state. A split is only worthwhile if the earlier pods actually packed well on the candidate tier.
3. **Displaced-pod eligibility** — no displaced pod may carry a required hostname-scoped constraint that the displaced-pod cost estimator cannot price correctly. Two cases are rejected (see `displacedCarriesHostFanOut`):
   - **Self-selecting fan-out.** `requiredDuringSchedulingIgnoredDuringExecution` pod anti-affinity on `kubernetes.io/hostname` whose selector matches the pod's own labels, or a `DoNotSchedule` topology spread on hostname with the same self-match. Either forces one-per-node fan-out among matching siblings, so the displaced cohort needs one fresh NodeClaim *per pod* rather than the single one the estimator prices.
   - **Unreachable affinity target.** `requiredDuringSchedulingIgnoredDuringExecution` pod affinity on hostname whose target label selector matches no other pod in the displaced cohort. The target is anchored to a locked survivor NodeClaim, so the displaced pod has no compatible host in the re-queue and becomes a scheduling failure. Self-selecting or sibling-satisfiable pod affinity is safe: the target rides along and co-locates on the fresh claim.

   Cross-label anti-affinity (e.g. "don't sit with `app=X`" from a pod labeled `app=Y`) is intentionally *not* rejected — on a fresh empty NodeClaim there is no sibling to violate against, so the estimator prices it correctly. Preferred (`PreferredDuringScheduling…`) variants are likewise not checked: the solver relaxes them, so they cannot turn into a scheduling failure.
4. **Net savings** — after estimating the cost of placing the displaced pods on a fresh NodeClaim, the *net* savings (`current − candidate − displacedEstimate`) must exceed both `costEpsilon` (float rounding guard for when minNetSavingsRatio is set to 0) and `minNetSavingsRatio` (3% of current price). There is no need to change the original NodeClaim if the savings expected is close to 0.

If any gate fails, the NodeClaim is left as-is and locked from any further optimizations during this scheduling run.

### Revert

`NodeClaim.RevertTo(index)` rolls the claim back to `schedulingOptions[index]` and returns the displaced pods. Every side effect that `Add` produced on behalf of those pods is reversed symmetrically:

- **Local NodeClaim fields** (`Pods`, `InstanceTypeOptions`, `Requirements`, `Spec.Resources.Requests`) are restored from the snapshot.
- **Topology counts** are reversed pod-by-pod using deltas stored at Add time. `Topology.Record` now returns a `[]TopologyDelta` describing exactly which topology groups had which domains incremented for the pod; `Topology.Unrecord` replays those deltas. Re-deriving the mutation from the NodeClaim's current requirements would be wrong — those requirements tighten monotonically across Adds, so by revert time they no longer match what `Record` saw.
- **Host-port reservations** are cleared via `HostPortUsage.DeletePod` per displaced pod.
- **Reserved offerings** are reconciled to the *intersection* of those held now and those reachable through the snapshot's instance types. Seats acquired for displaced pods return to the shared pool; seats retained on the locked claim stay. The reserved set is not monotonic in either direction, so a simple diff would be incorrect.

### Reserved-offering safety

Splitting a NodeClaim that holds a reserved offering fragments the reservation's accounting: the intersection rule retains the seat on the locked claim, and the fresh NodeClaim built from displaced pods cannot re-reserve it. In `ReservedOfferingModeStrict`, `offeringsToReserve` promotes "no compatible reservation" into a scheduling failure. `findSplitPoint` therefore short-circuits to "no split" whenever strict mode is active *and* the claim holds any reserved offering. This is deliberately conservative: correctness over savings.

Fallback mode has a quieter version of the same problem — the displaced-pod cost estimate can over-count the reservation as still available — but the savings-accuracy gap does not produce incorrect scheduling, so it is tracked as a follow-up rather than blocked here.

### Re-queueing displaced pods

Displaced pods collected across all splitting NodeClaims are fed into a single fresh `Queue` using the scheduler's standard pod ordering. They then flow back through `Solve`'s normal placement path.

### Feature gate and integration

- New `FeatureGates.NodeClaimOptimization` in `pkg/operator/options/options.go`, default `false`, parsed from the standard `FEATURE_GATES` env/flag string.
- `Provisioner.NewScheduler` appends `scheduling.EnableNodeClaimOptimization` to the scheduler's option list when the gate is on. Production callers use the gate; tests and direct integrators can set the option directly.
- The disruption controller's simulated scheduler is intentionally *not* wired to the gate. See **Simulated-scheduler carve-out** below — running a NodeClaim-splitting pass inside a consolidation simulation would turn valid single-replacement plans into discarded multi-NodeClaim plans.

## Observability

`Scheduler.OptimizationSnapshot` holds `Pre`/`Post` NodeClaim summaries and `PreCost`/`PostCost` totals for a single `Solve` call. It is exposed primarily for test invariants (`PostCost ≤ PreCost + costEpsilon`) but is also available to callers that want to log savings. A single summary log line is emitted per `Solve` when the pass ran.

## Considerations

### Opt-in by default

The pass is gated off for two reasons:

1. It changes `Solve` output for pre-existing workloads. Even though optimized cost is bounded below baseline, the resulting NodeClaim set differs, which can invalidate downstream assumptions held by specific deployments.
2. The correctness surface is large — shared state reversal, reservation reconciliation, topology deltas — and the rapid tests, not production, are the primary consumer of the guarantees today.

A feature gate allows cloud-provider distributions and early adopters to opt in without committing core Karpenter to the default-on behavior.

### Correctness over savings

The load-bearing invariant is that the total cost of the new NodeClaim set produced by `Solve` after the pass must be less than or equal to the cost of that same set before the pass (within `costEpsilon` for float rounding). Every decision below exists to preserve that invariant:

- The strict-mode reservation short-circuit sacrifices potential savings to avoid turning a utilization regression into a scheduling failure.
- `costEpsilon` and `minNetSavingsRatio` prevent near-equal costs from being mis-classified as savings, which matters most when pass-level composition (multiple claims' displaced pods merging into a third claim) makes the per-claim estimate optimistic.
- The claim is *locked* once inspected. `CanAdd` rejects further pods, so displaced pods re-queued by this pass cannot land back on the claim they were just split off of, and a subsequent pass cannot revisit the claim and accidentally undo a decision that was correct at the time.

The invariant is enforced by property-based tests that drive random workloads and assert `optimized cost ≤ baseline cost`. These tests are the canonical gate for tuning changes: a workload they find where the property fails is a bug regardless of rarity.

### Snapshot + per-pod reversal (rather than a replay log)

An alternative revert primitive was considered: record every `Add` as a log entry and reverse by popping entries and calling each side effect's inverse. Rejected because NodeClaim-local fields snapshot cleanly, and reservations cannot be reversed per-pod anyway (they are per-hostname). The log would duplicate information and make the common (non-optimization) `Add` path more expensive.

The chosen shape — snapshot local fields, reverse shared state per pod using deltas recorded at Add time, reconcile reservations once from the snapshot — keeps the non-optimization `Add` path effectively unchanged and puts the full reversal logic in one file that a reader can audit end-to-end.

### Topology deltas instead of re-derivation

An earlier iteration of the revert called `Topology.Unrecord(pod, requirements, …)` and re-derived which domains to decrement by walking the forward guards in reverse with the *current* `n.Requirements`. That only works for the last pod added. For every earlier displaced pod, `Record` saw looser requirements than are live at revert time, so re-derivation either decrements the wrong domain or decrements a counter that was never incremented.

Recording the mutation at Add time and replaying it at Revert time makes `Record` self-documenting — the returned value *is* the record of what it did — and decouples the inverse from future changes to `Record`'s guards.

The return-type change on `Topology.Record` is source-compatible: Go allows callers to discard the return value silently, and the only other in-tree caller (`ExistingNode.Add`) doesn't need the deltas because existing nodes are never reverted.

### Bounded passes (`maxOptimizationPasses = 2`)

Pass 1 captures the headline case: a NodeClaim whose tier stepped up past the point where the earlier pods wanted to be. Pass 2 cleans up residuals: displaced pods from two claims can merge onto a fresh claim and exhibit the same pattern. Because cloud instance sizing roughly doubles per tier, the value recovered by each successive pass decreases exponentially, so two passes is a deliberate trade-off between scheduling latency and recoverable savings. Raising the limit is trivial if evidence warrants it; there is no correctness concern with more passes beyond wall-clock cost.

### Tuning constants

All tuning constants live as a single `const` block in `nodeclaim_optimization.go`:

| Constant | Value | Purpose |
|---|---|---|
| `minPriceSavingsRatio` | 0.24 | Filters out marginal price differences; roughly "one tier down." |
| `minEfficiencyGain` | 0.10 | Requires the candidate to actually pack better, not just be cheaper. |
| `minNetSavingsRatio` | 0.03 | Rejects splits whose net savings are thin enough to be erased by composition effects. |
| `memoryGiBToCPURatio` | 10.0 | Weighted-resource ratio, derived from EC2 general-purpose external pricing. |
| `maxOptimizationPasses` | 2 | Bounds Solve-level wall-clock cost. |
| `costEpsilon` | 0.0001 | Float-rounding guard on cost comparisons. |

The memory/CPU ratio is acknowledged as cloud-specific. It is derived from EC2 general-purpose external pricing; all in-repo consumers (KWOK reference provider, rapid tests, AWS general-purpose families) fall inside the same band, so today it is a constant. A future iteration could derive it per-NodePool from the actual offering prices in the instance-type list.

### Simulated-scheduler carve-out

The disruption controller also runs the scheduler in simulation mode (consolidation, drift, emptiness). The optimization pass is not enabled there, for two reasons:

1. **Consolidation rejects multi-NodeClaim replacements.** `pkg/controllers/disruption/consolidation.go` discards any simulation whose `Results.NewNodeClaims` has more than one entry ("we're not going to turn a single node into multiple candidates"). The optimization pass is specifically a NodeClaim-splitting pass — running it inside a consolidation simulation would turn valid single-replacement plans into zero-replacement ones, erasing consolidation opportunities rather than improving them.
2. **Consolidation already cost-minimizes.** Its own replacement logic picks the cheapest fitting instance type; layering a second optimization pass on top would obscure which component made which decision without recovering savings consolidation itself cannot find.

