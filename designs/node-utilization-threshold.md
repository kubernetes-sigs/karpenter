# Node Utilization Threshold for Consolidation

## Motivation

Karpenter's consolidation logic evaluates every node as a potential disruption candidate, regardless of how well-utilized it is. This leads to a well-documented pattern of unnecessary node churn: highly-utilized nodes are disrupted and replaced, only for the replacement to be disrupted again shortly after. The problem is especially acute when nodes are running close to capacity -- the consolidation controller still considers them "underutilized" because a slightly cheaper instance type exists, even though the workload cannot practically fit on the cheaper alternative.

This behavior has been reported across multiple community issues:

- [#2319](https://github.com/kubernetes-sigs/karpenter/issues/2319) -- **Constant Underutilized eviction on highly allocated nodes**: A node at 96% CPU and 96% memory utilization is repeatedly disrupted despite no cheaper alternative being viable. Production pods are evicted in a loop.
- [#1117](https://github.com/kubernetes-sigs/karpenter/issues/1117) -- **Able to define a Consolidation WhenUnderutilized threshold percentage**: Feature request for a configurable utilization threshold to define what constitutes "underutilized". Closed as duplicate of #735.
- [#735](https://github.com/kubernetes-sigs/karpenter/issues/735) -- **Consolidation TTL**: Rapid node churn from consolidation, especially in clusters with frequent cron jobs. Led to the `consolidateAfter` feature, but does not address the root cause of well-utilized nodes being disrupted.
- [#1019](https://github.com/kubernetes-sigs/karpenter/issues/1019) -- **Rapid node churn**: Consolidation replaces 2 nodes with 1 every ~4 minutes, followed by immediate re-provisioning, creating a wasteful cycle.
- [#1686](https://github.com/kubernetes-sigs/karpenter/issues/1686) -- **Threshold usage percentage**: Request for a configurable threshold to prevent consolidation of well-utilized nodes. Reports that more-utilized nodes are consolidated over less-utilized ones.
- [#1430](https://github.com/kubernetes-sigs/karpenter/issues/1430) -- **Configurable Karpenter nodepool consolidation policies**: Single-node consolidation causes unproductive churn in large nodepools without meaningful cost savings.
- [#2705](https://github.com/kubernetes-sigs/karpenter/issues/2705) -- **consolidateAfter not working as expected**: The timing-based `consolidateAfter` setting does not prevent frequent disruptions of well-packed nodes.
- [aws/karpenter-provider-aws#3577](https://github.com/aws/karpenter-provider-aws/issues/3577) -- **Consolidation constantly replaces the same node type**: Consolidation repeatedly replaces the same instance types, stopping only when consolidation is disabled entirely.

## Proposal

Introduce a configurable **scale-down utilization threshold** that prevents nodes above a given resource utilization level from being considered as consolidation disruption candidates. This is a simple, opt-in mechanism that gives cluster operators a knob to protect well-utilized nodes from unnecessary churn.

### Configuration

| Method | Name | Default | Description |
|--------|------|---------|-------------|
| CLI flag | `--scaledown-utilization-threshold` | `0.75` | Nodes with average CPU+Memory utilization above this ratio are skipped for consolidation |
| Env var | `SCALE_DOWN_UTILIZATION_THRESHOLD` | `0.75` | Same as above, via environment variable |

The value must be between `0.0` and `1.0`:
- `0.75` (default): nodes using more than 75% of their allocatable CPU+Memory (by requests) are not considered for consolidation
- `1.0`: disables the feature entirely (all nodes are eligible for consolidation, preserving existing behavior)
- `0.0`: effectively disables consolidation for all non-empty nodes

### Behavior

The utilization check is applied during candidate creation in `NewCandidate()`, after all existing validation (node disruptability, pod disruptability, PDB checks). A node is **skipped** for consolidation when all three conditions are true:

1. **Utilization exceeds threshold**: The node's average CPU and memory utilization (pod requests / allocatable) is above the configured threshold
2. **Significant lifetime remaining**: The node has more than 10% of its `expireAfter` lifetime remaining (nodes near end-of-life should still be consolidated)
3. **Not drifted**: The node does not have the `Drifted` status condition (drifted nodes must always be eligible for disruption regardless of utilization)

### Utilization Calculation

Utilization is computed as the average of CPU and memory request-to-allocatable ratios:

```
utilization = (cpu_requests / cpu_allocatable + memory_requests / memory_allocatable) / 2
```

- Uses pod **requests** (not actual usage), consistent with Karpenter's scheduling model
- Only considers CPU and memory (not ephemeral storage, GPUs, etc.)
- Returns `0.0` for empty nodes (no pod requests)
- Handles edge cases where allocatable is zero for a resource type

### Interaction with Existing Features

| Feature | Interaction |
|---------|-------------|
| `consolidateAfter` | The utilization threshold is checked **after** `consolidateAfter` timing. A node must first pass the timing gate, then the utilization gate. |
| `WhenEmpty` consolidation policy | Empty nodes have utilization `0.0` and will always pass the threshold check. |
| Drift | Drifted nodes bypass the utilization threshold entirely. Drift-based disruption is safety-critical and must not be blocked. |
| `expireAfter` | Nodes with less than 10% lifetime remaining bypass the utilization threshold, ensuring expiring nodes are still consolidated efficiently. |
| `do-not-disrupt` annotation | Checked before the utilization threshold. Annotated nodes are already excluded. |
| PDB checks | Checked before the utilization threshold. Nodes blocked by PDBs are already excluded. |
| Disruption budgets | Applied at the orchestration level, after candidate selection. The utilization threshold reduces the candidate pool but does not affect budget enforcement. |

## Implementation

### Files Modified

1. **`pkg/utils/env/env.go`** -- Added `WithDefaultFloat64` helper to parse float64 environment variables
2. **`pkg/operator/options/options.go`** -- Added `ScaleDownUtilizationThreshold` field, CLI flag, and validation
3. **`pkg/test/options.go`** -- Added test option field for `ScaleDownUtilizationThreshold`
4. **`pkg/controllers/state/statenode.go`** -- Added `Utilization()` method to `StateNode`
5. **`pkg/controllers/disruption/types.go`** -- Added utilization threshold gate in `NewCandidate()`
