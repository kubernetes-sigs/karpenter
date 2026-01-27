# Consolidation Grace Period

## Summary

This proposal introduces a new `consolidationGracePeriod` field to the NodePool `Disruption` spec to address excessive node churn caused by consolidation cycles. The feature makes nodes "invisible" to the consolidation algorithm for a specified duration after any pod event, allowing nodes to "settle" before being considered for consolidation.

**Key Benefits:**
- Breaks consolidation cycles that cause constant node churn
- Simple and predictable behavior - no complex cost/utilization calculations
- Works alongside existing `consolidateAfter` mechanism
- Opt-in feature with no breaking changes

## Problem Statement

The `consolidateAfter` feature (implemented in [PR #1453](https://github.com/kubernetes-sigs/karpenter/pull/1453)) enables disruption of nodes based on a time duration after the last pod event. However, this implementation creates a problematic cycle of node churn in certain scenarios.

### The Problem

With `consolidateAfter`, a node can only be a consolidation **SOURCE** (have its pods moved away) after the timer expires. However, it can still be a **DESTINATION** (receive pods from other consolidations) at any time.

This creates a problematic feedback loop:

1. **Node_A** passes `consolidateAfter`, becomes a consolidation source
2. **Consolidation** moves pods from Node_A to **Node_B**
3. **Node_B** receives pods, its `consolidateAfter` timer resets (can't be source for 30s)
4. But **Node_B can still receive MORE pods** from other consolidation actions!
5. More pods move to Node_B from Node_C
6. Node_B's timer resets **AGAIN**
7. Cycle continues - nodes keep receiving pods without settling

### User Impact

Users report experiencing:
- Approximately daily periods of high workload node volatility caused by consolidation disruptions
- Large proportions of workload nodes getting disrupted and replaced in short periods
- Newly-created nodes running for only 5-10 minutes before being disrupted
- Constant node cycling even with relatively static workloads

**Reference Issue**: [aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)

## Proposed Solution

Introduce a `consolidationGracePeriod` field that makes a node **completely invisible** to the consolidation algorithm after any pod event (add/remove).

### Key Design Principle: Node Invisibility

When a node has a pod added or removed, it becomes **invisible** to consolidation for `consolidationGracePeriod` duration:

- **Cannot be a consolidation SOURCE** (pods can't be moved out)
- **Cannot be a consolidation DESTINATION** (pods can't be moved in)
- **Timer resets** every time there is a pod event

This is different from `consolidateAfter` which only affects whether a node can be a source.

### How It Works

```
Pod Event on Node → Node INVISIBLE to consolidation for consolidationGracePeriod
                  ↓
          Another pod event? → Timer RESETS
                  ↓
          Timer expires → Node visible to consolidation again
```

**The node is invisible means:**
- Consolidation can't move pods **INTO** this node
- Consolidation can't move pods **OUT OF** this node
- It's as if the node doesn't exist for consolidation calculations

### Examples

**Example 1: Node receives pods**
```
T - 10s    → Nodes running: Node_A, Node_B & Node_C.
T + 0s     → Node_D is added (pod scaled up). Node_D is INVISIBLE for 30min.
T + 10s    → Consolidation runs and only sees Node_A, Node_B & Node_C.
T + 30min  → Node_D becomes visible, consolidation can use it.
T + 30min10s → Consolidation runs, moves pods from Node_A to Node_D, terminates Node_A.
```

**Example 2: Timer resets on pod events**
```
T + 0s     → Node_C has pod added. Node_C INVISIBLE for 30min.
T + 10s    → Consolidation runs, only sees Node_A & Node_B.
T + 29min  → Node_C has pod removed. Timer RESETS - invisible for another 30min.
T + 30min10s → Consolidation runs, only sees Node_A & Node_B.
T + 59min  → Node_C visible again.
T + 59min10s → Consolidation runs, can now see Node_C.
```

**Example 3: Empty node can still be terminated**
```
T + 0s     → Node_D added (pod scaled up). Node_D INVISIBLE for 30min.
T + 10min  → Pod removed from Node_D, becomes empty. Node_D terminated.
            (Empty node termination happens regardless of grace period)
```

### How It Differs from `consolidateAfter`

| Aspect | `consolidateAfter` | `consolidationGracePeriod` |
|--------|-------------------|---------------------------|
| **Purpose** | Determines when node can be SOURCE | Makes node completely INVISIBLE |
| **Source filtering** | ✅ Yes | ✅ Yes |
| **Destination filtering** | ❌ No | ✅ Yes |
| **Timer reset** | On any pod event | On any pod event |

These mechanisms work together:
- `consolidateAfter`: Ensures nodes aren't consolidated too quickly after pod events
- `consolidationGracePeriod`: Ensures nodes aren't used in ANY consolidation decision until settled

## Proposed Spec

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 30s
    consolidationGracePeriod: 30m   # Node invisible to consolidation for 30min after pod event
    budgets:
    - nodes: 10%
```

## Code Definition

```go
type Disruption struct {
    // ... existing fields ...
    
    // ConsolidationGracePeriod is the duration after a pod event (add/remove) during which
    // the node is invisible to the consolidation algorithm.
    // When a node has a pod added or removed, it becomes invisible to consolidation
    // for this duration. During this time, consolidation cannot:
    // - Move pods OUT of this node (node cannot be a consolidation source)
    // - Move pods INTO this node (node cannot be a consolidation destination)
    // The timer resets every time there is a pod event on the node.
    // This prevents consolidation churn by allowing nodes to "settle" after pod movements.
    // When replicas is set, ConsolidationGracePeriod is simply ignored
    // +kubebuilder:validation:Pattern=`^(([0-9]+(s|m|h))+|Never)$`
    // +kubebuilder:validation:Type="string"
    // +kubebuilder:validation:Schemaless
    // +optional
    ConsolidationGracePeriod NillableDuration `json:"consolidationGracePeriod,omitempty"`
}
```

## Implementation Details

### Core Logic

The implementation is simple - a helper function checks if a node is within its grace period:

```go
// IsWithinConsolidationGracePeriod checks if a node is within its consolidation grace period.
func IsWithinConsolidationGracePeriod(n *state.StateNode, nodePoolMap map[string]*v1.NodePool, clk clock.Clock) bool {
    // Get NodePool for this node
    nodePool := nodePoolMap[n.Labels()[v1.NodePoolLabelKey]]
    if nodePool == nil || nodePool.Spec.Disruption.ConsolidationGracePeriod.Duration == nil {
        return false
    }
    
    gracePeriod := *nodePool.Spec.Disruption.ConsolidationGracePeriod.Duration
    
    // Get last pod event time (or initialization time as fallback)
    lastPodEvent := n.NodeClaim.Status.LastPodEventTime.Time
    if lastPodEvent.IsZero() {
        // Use initialization time as fallback
        initialized := n.NodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
        if initialized != nil && initialized.IsTrue() {
            lastPodEvent = initialized.LastTransitionTime.Time
        }
    }
    
    // Check if within grace period
    return clk.Since(lastPodEvent) < gracePeriod
}
```

### Integration Points

1. **GetCandidates (Source Filtering)**: Filter out nodes within grace period from being consolidation sources

2. **SimulateScheduling (Destination Filtering)**: Filter out nodes within grace period from being consolidation destinations

### RBAC Requirements

Uses existing Karpenter RBAC permissions - no additional permissions required.

### NodeClaim Status Field

Reuses the existing `LastPodEventTime` field on NodeClaim status, which is already updated by the `podevents` controller.

## Validation

- `consolidationGracePeriod` must be a valid duration string (e.g., "30m", "1h", "2h30m") or "Never"
- If set to "Never", consolidation grace period is disabled (same as not setting the field)
- The field is optional - if not set, behavior matches current implementation

## Defaults

- **`consolidationGracePeriod`**: Not set (nil) - feature is opt-in
- When `consolidationGracePeriod` is not set, behavior is identical to current implementation

## Design Alternatives Considered

### Alternative 1: Cost-Based Protection (Previously Implemented)

Protect nodes that are "cost-optimal" based on Karpenter's consolidation algorithm.

**Why Changed:**
- Added complexity without solving the core issue
- The problem isn't about protecting "good" nodes - it's about letting nodes settle before any consolidation decision

### Alternative 2: Utilization-Based Protection

Protect nodes above a certain utilization threshold.

**Why Rejected:**
- **Conflicts with cost optimization**: Karpenter optimizes for cost, not utilization
- **Reserved instances**: Should be used regardless of utilization
- **Complexity**: Adds gates that conflict with core consolidation logic

### Alternative 3: Minimum Node Age

Prevent consolidation of nodes below a certain age.

**Why Rejected:**
- Doesn't address the destination problem - nodes can still receive pods
- Overlaps with existing `consolidateAfter` behavior

## Rationale

The proposed solution is chosen because:

1. **Simple**: Just make node invisible - no complex calculations
2. **Addresses root cause**: Prevents both source AND destination churn
3. **Predictable**: Easy to understand - pod event → invisible for X time
4. **Non-invasive**: Doesn't change consolidation algorithm, just filters nodes
5. **Opt-in**: Doesn't change behavior for existing users

## Migration Path

- **No migration required**: Feature is opt-in via new optional field
- **Existing NodePools**: Continue to work as before
- **New NodePools**: Can opt-in by setting `consolidationGracePeriod`

## Testing Considerations

### Unit Tests

- Test that nodes within grace period are filtered from candidates (sources)
- Test that nodes within grace period are filtered from destinations
- Test timer reset on pod events
- Test "Never" value
- Test nil/unspecified value (should behave like current implementation)

### Integration Tests

- Test consolidation cycle prevention
- Test that new nodes are invisible to consolidation
- Test that receiving pods keeps node invisible
- Test with various `consolidationGracePeriod` durations

## References

- [PR #1453: feat: implement consolidateAfter](https://github.com/kubernetes-sigs/karpenter/pull/1453)
- [Issue #7146: Karpenter "Underutilised" disruption causing excessive node churn](https://github.com/aws/karpenter-provider-aws/issues/7146)
- [Design: Disruption Controls](./disruption-controls.md)
