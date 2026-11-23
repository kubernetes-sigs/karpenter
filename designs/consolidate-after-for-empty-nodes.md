# Separate consolidateAfter for Empty Nodes

## Motivation

`spec.disruption.consolidateAfter` is one timer for every consolidation reason. A node becomes a consolidation candidate once that long has passed since its last pod event (or since it initialized), whether it is empty or still running pods. Since [#3006](https://github.com/kubernetes-sigs/karpenter/pull/3006) the same timer also stops pods from being scheduled onto a node that is still inside its window during consolidation.

Empty and underutilized nodes are not equally costly to act on. An empty node is pure cost and deleting it evicts nothing, so operators want it gone quickly. An underutilized node has pods that will be evicted, so operators want a longer wait that rides out transient dips. With one timer the operator has to choose a compromise. This is [#3046](https://github.com/kubernetes-sigs/karpenter/issues/3046), which was accepted with `needs-design`. Budgets already scope by reason (`budgets[].reasons`), but they limit how many nodes can be disrupted at once, not when a node becomes eligible.

Related open work this RFC has to live with:

- [#3105](https://github.com/kubernetes-sigs/karpenter/issues/3105) (accepted, backlog) proposes improving or replacing the `consolidateAfter` timer with a regret heuristic. `emptyAfter` is a small knob on today's timer and does not block that direction (see Alternative 5).
- [#3185](https://github.com/kubernetes-sigs/karpenter/issues/3185) and [#3147](https://github.com/kubernetes-sigs/karpenter/issues/3147) report nodes disrupted inside their `consolidateAfter` window. A short empty timer makes those bugs easier to hit, so this RFC calls them out under Edge Cases.
- [#1921](https://github.com/kubernetes-sigs/karpenter/issues/1921) (ignoring specific pods when resetting the timer) is a separate request and stays out of scope.

### Use Cases

1. **CI or batch pool.** Jobs finish and leave empty nodes. The operator wants them reclaimed in 30 seconds. The same pool also runs long-lived runners that should only be repacked after 15 minutes of stable utilization.
2. **Spiky service.** The operator sets `consolidateAfter: 30m` so capacity is held through traffic dips and rolling deploys. A node that has been fully empty for that long is not worth keeping for another dip, so the operator wants a shorter empty timer.
3. **Balanced policy.** `Balanced` already deletes empty nodes through Emptiness. Operators using it want to tune empty reclamation without loosening the gate on moves that evict pods.

### Non-Goals

- Per-reason timers for `Drifted` or `Unhealthy`. Neither uses `consolidateAfter` today.
- Per-node or per-pod timers.
- Changing the default or the meaning of `consolidateAfter` for pools that do not set the new field.
- Changing which nodes the destination check from #3006 protects.
- Changing what counts as empty. A node is empty when `Candidate.IsEmpty` is true, meaning no pod contributes positive reschedule disruption cost. That includes nodes running only DaemonSet pods or other zero-cost pods, and matches the cost model in [#2942](https://github.com/kubernetes-sigs/karpenter/pull/2942).

## Proposal

Add an optional `emptyAfter` field next to `consolidateAfter`. When set, it replaces `consolidateAfter` for empty nodes. When unset, nothing changes.

### Proposed Spec

```yaml
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 15m   # non-empty nodes
    emptyAfter: 30s         # empty nodes
```

```go
type Disruption struct {
	ConsolidateAfter NillableDuration `json:"consolidateAfter,omitempty"`
	// EmptyAfter is the duration the controller waits before deleting an empty node.
	// When unset, ConsolidateAfter applies to empty nodes as well.
	// When replicas is set, EmptyAfter is ignored.
	// +kubebuilder:validation:Pattern=`^(([0-9]+(s|m|h))+|Never)$`
	// +kubebuilder:validation:Type="string"
	// +kubebuilder:validation:Schemaless
	// +optional
	EmptyAfter *NillableDuration `json:"emptyAfter,omitempty"`
	// ...
}
```

The field is a pointer because `NillableDuration` already uses a nil duration to mean `Never`, so "unset" needs its own representation.

### How It Works

Effective timers for a NodePool:

- Empty node: `emptyAfter` if set, otherwise `consolidateAfter`.
- Non-empty node: `consolidateAfter`.

The clock start is unchanged: `lastPodEventTime` if set, otherwise the `Initialized` transition. Removing the last pod is a pod event, so the empty timer starts when the node becomes empty.

Emptiness is only known to the disruption methods, which already list each node's pods when they build candidates. The `nodeclaim/disruption` controller sets `Consolidatable` per NodeClaim and has no pod list, so it cannot choose between the two timers. The change splits the work:

1. The `Consolidatable` condition is set once the **shorter** of the two effective timers has elapsed, using the same requeue logic as today. If both timers are `Never`, the condition is cleared as it is today.
2. `Emptiness.ShouldDisrupt` additionally requires that the node is not still inside its empty timer.
3. `Consolidation.ShouldDisrupt` additionally requires that the node is not still inside `consolidateAfter`.

`Consolidatable` is read in exactly two places, `Emptiness.ShouldDisrupt` and `Consolidation.ShouldDisrupt`, so shortening it does not change any other behavior.

Both checks use one shared helper in `pkg/utils/disruption`, next to `IsUnderConsolidateAfter`, which takes the timer to apply. The destination check from #3006 keeps using `consolidateAfter`.

### Interaction with Existing Features

- **`WhenEmpty`:** only Emptiness runs, so `consolidateAfter` acts as the empty timer today. `emptyAfter` overrides it when set.
- **`Balanced`:** Emptiness runs under every policy, so `emptyAfter` applies to its empty deletions. Pod-evicting moves keep `consolidateAfter`.
- **Budgets:** unchanged. Empty and underutilized deletions are already budgeted under separate reasons.
- **Static NodePools:** `emptyAfter` is ignored when `replicas` is set, like `consolidateAfter`.
- **Drift hash:** `Disruption` is not part of the NodeClaim template hash, so the new field causes no drift.

### Observability

This RFC proposes that the `Unconsolidatable` event and the debug log for the requeue name which timer a node is waiting on. No new metric or condition is added.

### Edge Cases

- **Node empties, then pods arrive.** The pod event resets the clock and the node is no longer empty, so `consolidateAfter` applies again.
- **Node has been empty for 20s with `emptyAfter: 30s`, `consolidateAfter: 15m`.** `Consolidatable` becomes true at 30s. Emptiness deletes the node at 30s. Consolidation skips it as empty.
- **Node with pods, same pool, idle for 30s.** `Consolidatable` is already true, but Consolidation sees it is still inside `consolidateAfter` and skips it until 15m.
- **`emptyAfter` longer than `consolidateAfter`.** Allowed. `Consolidatable` follows the shorter timer and Emptiness holds the node for the longer one.
- **Short timers and event dedupe.** The pod events controller drops an update if `lastPodEventTime` was written less than 10 seconds ago, so the recorded time can trail the real last pod event by up to 10 seconds. With a very short `emptyAfter` (for example 5s) a node can be deleted sooner than the operator expects. The field docs should note that values near 10s can fire up to 10 seconds early.
- **Existing timer bugs.** #3185 (a pod binding during validation) and #3147 (the timer not resetting under concurrent pod deletion) also affect the empty path. They live in the shared timer and exist without this change, but a short `emptyAfter` raises their cost. #3185 has a fix in flight in [#3188](https://github.com/kubernetes-sigs/karpenter/pull/3188), which should land first.
- **Expiry of the longer timer.** No condition changes when the longer timer elapses, so nothing marks the cluster unconsolidated. The existing five-minute revalidation in `Cluster.ConsolidationState` bounds the delay, but a node can wait up to five minutes past its timer. See open question 3.

## Alternatives Considered

### Alternative 1: Per-reason map in `consolidateAfter`

`consolidateAfter: {Empty: 30s, Underutilized: 15m}`. This changes the type of an existing v1 field from a duration string to an object, which breaks existing manifests. A sibling field such as `consolidateAfterByReason` avoids that, but then two fields express one setting and the precedence rules need defining.

### Alternative 2: Budget-style list

`timers: [{reasons: [Empty], after: 30s}]`. It matches `budgets[].reasons` and could later cover `Drifted`. For two reasons it is more API than needed, and it needs precedence rules against `consolidateAfter`. It can be added later without conflicting with `emptyAfter`.

### Alternative 3: A second status condition

An `Emptiable` condition set by `nodeclaim/disruption` would make each timer a clean condition transition and fix the wake-up in open question 3. It needs the controller to know whether a node is empty, which means listing pods for every NodeClaim on every reconcile. The disruption methods build candidates once per loop with that cost already paid, so this RFC keeps emptiness there.

### Alternative 4: Split into two NodePools

This does not work. Empty and underutilized describe the same node at different points in its life, so a node would have to move between pools.

### Alternative 5: Wait for the regret heuristic in #3105

If `consolidateAfter` is replaced by a learned heuristic, a separate empty timer may be unnecessary. That work is `backlog` and `needs-design`, with no owner. `emptyAfter` costs one optional field and one helper. If #3105 lands, `emptyAfter` can become a lower bound on empty reclamation or be deprecated, and unset pools are unaffected either way.

## Backward Compatibility

The field is additive and optional. Unset behaves exactly as today. No conversion webhook change is needed. NodePools using the field on a Karpenter version that predates it have it pruned by the CRD schema, which restores the single-timer behavior.

## Graduation Criteria

Ship without a feature gate. Unset is a no-op and the field is a bounded timer, so the risk is a misconfigured timer on one NodePool. If maintainers prefer, mark the field alpha in its doc comment as `replicas` is.

## Open Questions

1. **Name.** `emptyAfter` is the shortest. `consolidateAfterEmpty` keeps the `consolidateAfter` prefix and sorts next to it. Which fits the v1 API conventions?
2. **`Never` for `emptyAfter`.** It would mean empty nodes are never deleted by Emptiness while underutilized moves still run. That is a rare setup and easy to misconfigure. Allow it, or reject it in validation?
3. **Waking up on the longer timer.** Accept up to five minutes of lateness from the periodic revalidation, or have `nodeclaim/disruption` mark the cluster unconsolidated when the longer timer elapses?
4. **Destination check.** Should a node that is inside `emptyAfter` but past `consolidateAfter` count as inside its window for #3006? This RFC says no, since `consolidateAfter` is the timer that protects pods from churn.
