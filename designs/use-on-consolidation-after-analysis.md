# Consolidation Grace Period - Critical Analysis

## The Problem: Consolidation Churn

With `consolidateAfter`, Karpenter can consolidate nodes after a quiet period. But this creates a cycle:

```
Node_A stable → consolidated → pods move to Node_B → Node_B resets timer
                             → Node_B receives MORE pods from Node_C
                             → Node_B never settles → CHURN
```

**Key Insight**: `consolidateAfter` only prevents a node from being a consolidation **SOURCE**. It doesn't prevent it from being a **DESTINATION**. This is the gap that causes churn.

## The Solution: Node Invisibility

`consolidationGracePeriod` makes a node **completely invisible** to consolidation after any pod event:

- **Cannot be SOURCE** (pods can't move out)
- **Cannot be DESTINATION** (pods can't move in)
- **Timer resets** on every pod event

```
Pod event → Node invisible for consolidationGracePeriod
         → Another pod event? Timer resets
         → Timer expires? Node visible again
```

## How `consolidateAfter` and `consolidationGracePeriod` Differ

| Aspect | `consolidateAfter` | `consolidationGracePeriod` |
|--------|-------------------|---------------------------|
| Node as SOURCE | ✅ Protected | ✅ Protected |
| Node as DESTINATION | ❌ Not protected | ✅ Protected |
| Purpose | When can node be consolidated? | When can node participate in consolidation? |

## Implementation

The implementation is simple - we filter nodes that are within their grace period from both:

1. **GetCandidates**: Filter from consolidation sources
2. **SimulateScheduling**: Filter from consolidation destinations

```go
func IsWithinConsolidationGracePeriod(n *state.StateNode, ...) bool {
    gracePeriod := nodePool.Spec.Disruption.ConsolidationGracePeriod.Duration
    lastPodEvent := n.NodeClaim.Status.LastPodEventTime.Time
    return clock.Since(lastPodEvent) < gracePeriod
}
```

## Example Timeline

```
Config: consolidateAfter=30s, consolidationGracePeriod=30m

T=0       Node_A, Node_B, Node_C exist
T=1min    Node_D created (pod scheduled)
          → Node_D INVISIBLE to consolidation for 30min
          
T=1min30s Consolidation runs
          → Only sees Node_A, Node_B, Node_C
          → Node_D doesn't exist for this calculation
          
T=5min    Pod removed from Node_D
          → Timer RESETS → invisible for another 30min
          
T=35min   Node_D grace period expires
          → Node_D now visible to consolidation
          
T=35min30s Consolidation runs
           → Sees all nodes including Node_D
           → Normal consolidation logic applies
```

## Key Design Decisions

### Why Not Cost-Based or Utilization-Based?

Previous iterations tried to protect "cost-optimal" or "high-utilization" nodes. This was rejected because:

1. **Complexity**: Adds gates that conflict with Karpenter's consolidation logic
2. **Wrong problem**: The issue isn't about protecting "good" nodes
3. **Root cause**: The real problem is nodes not getting a chance to settle

### Why "Invisible" Instead of Just "Protected"?

Making a node invisible to consolidation (both source AND destination) solves the root cause:

- Nodes get time to settle after pod movements
- No continuous pod shuffling to "settling" nodes
- Simple and predictable behavior

## Drawbacks and Considerations

### Drawback 1: Delayed Consolidation

Nodes within grace period won't be consolidated even if they should be.

**Mitigation**: This is intentional - the whole point is to let nodes settle. Users can tune the grace period.

### Drawback 2: Suboptimal Pod Placement

Pods can't move TO nodes within grace period, potentially missing better placement.

**Mitigation**: Once grace period expires, normal consolidation will optimize placement.

### Drawback 3: Memory Overhead

Tracking grace period for all nodes.

**Mitigation**: No tracking needed! We calculate on-the-fly using existing `LastPodEventTime`.

## Configuration Recommendations

| Workload Type | Suggested `consolidationGracePeriod` |
|---------------|-------------------------------------|
| High churn (frequent scaling) | 30m - 1h |
| Medium churn | 15m - 30m |
| Low churn (stable) | 5m - 15m |
| Don't use | `Never` or don't set |

## Summary

The `consolidationGracePeriod` feature solves consolidation churn by making nodes "invisible" to consolidation after pod events. This is a simple, targeted solution that:

1. **Solves the root cause** - prevents destination churn, not just source churn
2. **Simple implementation** - just filter nodes, no complex logic
3. **Predictable behavior** - pod event → invisible for X time
4. **Opt-in** - no changes for users who don't set it
