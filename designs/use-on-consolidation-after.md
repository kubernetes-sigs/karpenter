# Consolidation Grace Period

## Summary

This proposal introduces a new `consolidationGracePeriod` field to the NodePool `Disruption` spec to address excessive node churn caused by consolidation cycles. The feature provides a grace period for stable nodes before re-evaluation for consolidation, breaking the feedback loop where nodes are repeatedly evaluated.

**Key Benefits:**
- Breaks consolidation cycles that cause constant node churn
- **Aligns with Karpenter's cost-based optimization** - doesn't add utilization gates
- Works alongside existing `consolidateAfter` mechanism
- Opt-in feature with no breaking changes
- Simple and predictable behavior

## Problem Statement

The `consolidateAfter` feature (implemented in [PR #1453](https://github.com/kubernetes-sigs/karpenter/pull/1453)) enables disruption of nodes based on a time duration after the last pod event. However, this implementation creates a problematic cycle of node churn in certain scenarios:

### Current Behavior

With `consolidateAfter`, Karpenter tracks `LastPodEventTime` on each NodeClaim. When pod events occur (pod scheduled, pod removed, pod goes terminal), this timestamp is updated. A node is only considered consolidatable if `timeSince(LastPodEventTime) >= consolidateAfter`.

### The Problem

This creates a problematic feedback loop:

1. **High pod churn nodes** constantly reset their `consolidateAfter` timer due to frequent pod events, making them "unconsolidatable"
2. **Stable, old nodes** that have passed their `consolidateAfter` duration become consolidation candidates
3. **Kubernetes default scheduling behavior** prefers scheduling pods to the least-utilized nodes, which are often the newer nodes with recent pod events
4. This creates a **continuous cycle**:
   - Old nodes get consolidated (pods moved to new nodes)
   - New nodes receive pods, resetting their `consolidateAfter` timer
   - New nodes become unconsolidatable
   - Old nodes (now with fewer pods) become consolidation candidates again
   - Cycle repeats

### User Impact

Users report experiencing:
- Approximately daily periods of high workload node volatility caused by consolidation disruptions (reason: Underutilised)
- Large proportions of workload nodes getting disrupted and replaced in short periods
- Newly-created nodes running for only 5-10 minutes before being disrupted as Underutilised
- Constant node cycling that leads to the same/similar number of nodes and types, even with only On-Demand instances
- Relatively static workloads in a nodepool of just on-demand instances leading to constant node churn

**Reference Issue**: [aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)

## Proposed Solution

Introduce a `consolidationGracePeriod` field in the NodePool `Disruption` spec that provides a grace period before re-evaluating stable nodes for consolidation.

### Key Design Principle: Align with Cost Optimization

**This feature does NOT add a utilization gate** that would conflict with Karpenter's cost-based consolidation. Instead, it adds a **cooldown period** to prevent repeated re-evaluation of the same nodes.

The insight is:
1. Karpenter's consolidation optimizes for **cost**, not utilization
2. If consolidation can find a cheaper configuration, it will act during the initial evaluation
3. The grace period prevents repeated re-evaluation of nodes that are already cost-optimal
4. This breaks the churn cycle without conflicting with cost optimization

### Protection Criteria

A node is protected from re-evaluation when **ALL** of the following are true:

1. ✅ `consolidationGracePeriod` is configured on the NodePool
2. ✅ Node has passed `consolidateAfter` (stable - no pod events for `consolidateAfter` duration)

**Protection Duration:** `consolidationGracePeriod` from the moment the node first became consolidatable.

### How It Breaks the Cycle

```
BEFORE (the problem):
┌─────────────────────────────────────────────────────────────────┐
│  Old Stable Node                                                │
│  ↓ becomes consolidatable (passed consolidateAfter)             │
│  ↓ Karpenter evaluates it for consolidation                     │
│  ↓ No cheaper option found OR pods moved to newer nodes         │
│  ↓ Newer nodes reset their consolidateAfter timer               │
│  ↓ Re-evaluation happens again and again                        │
│  └──────────────────→ CHURN REPEATS ←───────────────────────────┘

AFTER (with consolidationGracePeriod):
┌─────────────────────────────────────────────────────────────────┐
│  Old Stable Node                                                │
│  ↓ becomes consolidatable (passed consolidateAfter)             │
│  ↓ Consolidation evaluates it - finds no cost benefit           │
│  ↓ PROTECTED for consolidationGracePeriod duration              │
│  ↓ No repeated re-evaluation → CYCLE BROKEN                     │
│  ↓ OR receives new pods → LastPodEventTime resets naturally     │
└─────────────────────────────────────────────────────────────────┘
```

### How It Differs from `consolidateAfter`

- **`consolidateAfter`**: Determines when a node **becomes eligible** for consolidation (waits for a quiet period after pod events)
- **`consolidationGracePeriod`**: Provides a **cooldown** before re-evaluating stable nodes

These mechanisms work together:
- `consolidateAfter` ensures nodes aren't consolidated too quickly after pod events
- `consolidationGracePeriod` prevents repeated re-evaluation of already-evaluated nodes

### Why No Utilization Threshold?

An earlier design included a utilization threshold. This was **removed** based on maintainer feedback:

> "Focus on cost vs utilization since that's what Karpenter optimizes for and adding a utilization gate would be at odds with the core consolidation logic."

**Reasons:**
1. **Cost ≠ Utilization**: An m5.xlarge might be cheaper than m8.xlarge even at lower utilization
2. **Reserved Instances**: Pre-purchased capacity should be used regardless of utilization
3. **Spot Pricing**: Larger spot instances might be cheaper than smaller on-demand ones
4. **Simplicity**: A cooldown period is simpler and doesn't conflict with cost optimization

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
    consolidationGracePeriod: 1h   # New field - grace period before re-evaluation
    budgets:
    - nodes: 10%
```

## Code Definition

```go
type Disruption struct {
    // ... existing fields ...
    
    // ConsolidationGracePeriod is the duration the controller will wait
    // before re-evaluating a stable node for consolidation.
    // A node is protected when it has been stable (no pod events) for consolidateAfter duration.
    // When protected, the node will not be re-evaluated for consolidation
    // for the consolidationGracePeriod duration.
    // This prevents consolidation churn where stable nodes are repeatedly evaluated,
    // while respecting Karpenter's cost-based consolidation decisions.
    // If consolidation can find a cheaper configuration, it will act during the initial evaluation.
    // The grace period prevents repeated re-evaluation of nodes that are already cost-optimal.
    // When replicas is set, ConsolidationGracePeriod is simply ignored
    // +kubebuilder:validation:Pattern=`^(([0-9]+(s|m|h))+|Never)$`
    // +kubebuilder:validation:Type="string"
    // +kubebuilder:validation:Schemaless
    // +optional
    ConsolidationGracePeriod NillableDuration `json:"consolidationGracePeriod,omitempty"`
}
```

## Implementation Details

### RBAC Requirements

The consolidation observer controller requires the following Kubernetes RBAC permissions (already included in the core Karpenter ClusterRole):

```yaml
- apiGroups: ["karpenter.sh"]
  resources: ["nodepools", "nodepools/status", "nodeclaims", "nodeclaims/status"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["pods", "nodes"]
  verbs: ["get", "list", "watch"]
```

These permissions enable the observer to:
- Read NodePool configuration (`consolidationGracePeriod`, `consolidateAfter`)
- Read NodeClaim status (`LastPodEventTime`, `Initialized` condition)

### NodeClaim Status Field

We reuse the existing `LastPodEventTime` field on NodeClaim status, which is already updated by the `podevents` controller when:
- A pod is scheduled to the node
- A pod is removed from the node
- A pod scheduled to the node succeeds/fails (goes terminal)

### Consolidation Observer Controller

An independent observer controller (`pkg/controllers/nodeclaim/consolidationobserver/controller.go`) tracks nodes that are protected:

1. **Node has passed consolidateAfter**: `timeSince(LastPodEventTime) >= consolidateAfter`
2. **NodePool has consolidationGracePeriod configured**

When a node meets these criteria, it is added to a protected list with expiration time of `consolidatableTime + consolidationGracePeriod`.

### Consolidation Logic Integration

The consolidation controller checks the observer's `IsProtected()` method:
- If a node is protected, it is **not** marked as consolidatable
- A requeue is scheduled for when the protection expires

### Timeline Example

**Configuration:**
- `consolidateAfter: 30s`
- `consolidationGracePeriod: 5m`

```
Timeline for Node A (stable):
─────────────────────────────────────────────────────────────────────────
T=0      │ Node A created, pod scheduled
         │ Status: Protected by consolidateAfter (30s countdown)
         │
T=30s    │ consolidateAfter passes, node becomes "consolidation candidate"
         │ Consolidation evaluates: can it find a cheaper option?
         │ If YES → node gets consolidated (cost optimization works!)
         │ If NO → node is protected for consolidationGracePeriod (5m)
         │
T=5m30s  │ consolidationGracePeriod expires
         │ Status: Re-evaluate for consolidation
─────────────────────────────────────────────────────────────────────────
```

## Validation

- `consolidationGracePeriod` must be a valid duration string (e.g., "30m", "1h", "2h30m") or "Never"
- If set to "Never", consolidation grace period is disabled (same as not setting the field)
- The field is optional - if not set, behavior matches current implementation

## Defaults

- **`consolidationGracePeriod`**: Not set (nil) - feature is opt-in
- When `consolidationGracePeriod` is not set, behavior is identical to current implementation

## Examples

### Example 1: Breaking Consolidation Cycles

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: workload
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 30s
    consolidationGracePeriod: 1h  # Grace period before re-evaluation
```

**Scenario**: A cluster with ReplicaSets that scale up/down frequently
- **Without `consolidationGracePeriod`**: Stable nodes repeatedly evaluated → churn
- **With `consolidationGracePeriod: 1h`**: Stable nodes get a 1-hour grace period

### Example 2: Long-Running Stable Workloads

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: stable-workloads
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 5m
    consolidationGracePeriod: 4h  # Longer grace period for stable workloads
```

### Example 3: Disabling Grace Period

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 30s
    consolidationGracePeriod: Never  # Explicitly disable grace period
```

## Design Alternatives Considered

### Alternative 1: Utilization-Based Protection (Rejected)

Protect nodes above a certain utilization threshold.

**Why Rejected:**
- **Conflicts with cost optimization**: Karpenter optimizes for cost, not utilization
- **Reserved instances**: Should be used regardless of utilization
- **Instance pricing inversions**: Larger instances can be cheaper

### Alternative 2: Minimum Node Age

Prevent consolidation of nodes below a certain age.

**Pros:**
- Simpler implementation

**Cons:**
- Doesn't address the core issue - nodes can still be consolidated after the minimum age
- Overlaps with existing `consolidateAfter` behavior

### Alternative 3: Pod Stability Window

Track when pods on a node were last added/removed.

**Cons:**
- This is essentially what `consolidateAfter` already does

## Rationale

The proposed solution is chosen because:

1. **Aligns with cost optimization**: Doesn't add utilization gates that conflict with Karpenter's philosophy
2. **Addresses the root cause**: Prevents repeated re-evaluation of stable nodes
3. **Reuses existing infrastructure**: Leverages `LastPodEventTime` which is already tracked
4. **Simple and targeted**: One clear grace period mechanism
5. **Predictable behavior**: Easy to understand and debug
6. **Opt-in**: Doesn't change behavior for existing users

## Migration Path

- **No migration required**: Feature is opt-in via new optional field
- **Existing NodePools**: Continue to work as before
- **New NodePools**: Can opt-in by setting `consolidationGracePeriod`

## Testing Considerations

### Unit Tests

- Test that stable nodes ARE protected after passing consolidateAfter
- Test protection expiration after `consolidationGracePeriod` duration
- Test interaction between `consolidateAfter` and `consolidationGracePeriod`
- Test "Never" value
- Test nil/unspecified value (should behave like current implementation)

### Integration Tests

- Test consolidation cycle prevention
- Test that pod events reset protection (node becomes non-consolidatable)
- Test with various `consolidationGracePeriod` durations
- Test with ReplicaSet scale-up/down scenarios

## References

- [PR #1453: feat: implement consolidateAfter](https://github.com/kubernetes-sigs/karpenter/pull/1453)
- [Issue #7146: Karpenter "Underutilised" disruption causing excessive node churn](https://github.com/aws/karpenter-provider-aws/issues/7146)
- [Design: Disruption Controls](./disruption-controls.md)
- [Design: Spot Consolidation](./spot-consolidation.md)
