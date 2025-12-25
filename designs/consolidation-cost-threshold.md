# Consolidation Savings Threshold

## Definitions and Current Algorithm

Karpenter computes a **disruption cost** for each node to order consolidation candidates from easiest to hardest to disrupt. The complete formula (see `pkg/controllers/disruption/orchestration/queue.go`):

```
disruption_cost = sum(per_pod_cost) * lifetime_remaining
```

Where:

```
per_pod_cost = clamp(1.0 + pod_priority/2^25 + pod_deletion_cost/2^27, -10.0, 10.0)
```

Breaking down each term:

- **1.0 base per pod**: Ensures pod count contributes to cost even without explicit priority or deletion cost
- **pod_priority/2^25**: From the pod's PriorityClass (default 0). Dividing by 2^25 (~33M) normalizes the int32 range to roughly [-64, +64]
- **pod_deletion_cost/2^27**: From the `controller.kubernetes.io/pod-deletion-cost` annotation (default 0). Dividing by 2^27 (~134M) provides similar normalization
- **clamp to [-10, 10]**: Prevents any single pod from dominating the total cost. A node with 50 system-critical pods gets 50 * 10 = 500, not 50 * 61 = 3050
- **lifetime_remaining**: Fraction of time until `expireAfter`. A node at 50% of its lifetime has cost halved. Nodes without `expireAfter` have lifetime_remaining = 1.0

This formula produces a unitless heuristic. The absolute value is not directly interpretable. It exists to enable consistent comparisons: higher cost means harder to disrupt.

Example values (assuming lifetime_remaining = 1.0):
- Empty node: disruption cost = 0.0
- Node with 10 default-priority pods: 10 * 1.0 = 10.0
- Node with 10 system-cluster-critical pods (priority ~2B): 10 * 10.0 = 100.0 (each pod clamped to max)

The `lifetime_remaining` multiplier reduces effective disruption cost as nodes approach expiration:

```
Effective           |****
Disruption Cost     |    ****
                    |        ****
                    |            ****
                    |                ****
                    |                    ****
                    +----------------------------> Node Age
                    new                     expireAfter
```

The intuition: a node nearing expiration will be disrupted soon anyway (drift or expiration). Voluntary consolidation now has lower marginal cost because we're not "stealing" much remaining lifetime from the workloads.

## Problem Statement

The current consolidation algorithm fires whenever a cheaper alternative exists:

```
consolidate when: replacement_price < current_price
```

This causes excessive churn when savings are marginal. Users report cascading consolidation where nodes are replaced, then replacements are replaced within minutes, disrupting pods multiple times for negligible benefit.

**Case study: [aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)**

A production cluster with 14-17 nodes experienced cascading consolidation during off-peak hours:

- 7 nodes disrupted over 30 minutes, 15 total "Underutilised" events
- Replacement nodes ran 5-10 minutes before being disrupted again
- 2-3 generations of cascading consolidation
- Pods restarted up to 4 times in succession
- Net result: 1 xlarge replaced by 4 large nodes (more nodes, more complexity)

The algorithm saw opportunities like:

```
Node: m6a.large @ $0.086/hr, ~5 pods
Replacement: m7i-flex.large @ $0.080/hr
Savings: $0.006/hr

Current algorithm: 0.006 > 0? CONSOLIDATE
```

Six-tenths of a cent per hour. The user disrupted 5 pods, waited for graceful termination, rescheduled workloads, and then did it again 10 minutes later. The disruption cost dwarfed any savings.

The workarounds are painful: disabling spot-to-spot consolidation entirely, extensive PDB usage, or multi-hour `consolidateAfter` periods. Users shouldn't have to choose between "consolidation works" and "consolidation doesn't destroy my cluster."

This RFC builds on [spot-consolidation.md](spot-consolidation.md), which identified "Price Improvement Factor" as future work, and [PR #2562](https://github.com/kubernetes-sigs/karpenter/pull/2562), which proposed a percentage-based threshold. We adopt many sound decisions from that PR: NodePool-level configuration, a default of 0 that preserves existing behavior, and string-based CRD fields to avoid float serialization issues. The key change is the formula: instead of a flat percentage, we normalize savings by disruption cost.

## Proposed Solution

Require savings to justify disruption by normalizing savings against disruption cost.

**Modified algorithm:**

```
required_savings = threshold * disruption_cost
consolidate when: savings > 0 AND savings >= required_savings
```

The `savings > 0` check ensures we never consolidate when there's no cost reduction, even with threshold = 0.

**Modified disruption cost formula:**

```
disruption_cost = (node_base_cost + sum(per_pod_cost)) * lifetime_remaining
```

The new term:

- **node_base_cost = 1.0**: Ensures empty nodes have non-zero disruption cost. Without this, savings/cost is undefined for empty nodes. The value 1.0 is analogous to one default-priority pod, capturing operational overhead (connection draining, API server load) independent of pod count. See "Design Considerations" for rationale.

**Multi-node consolidation:**

When consolidating multiple nodes to one replacement, sum the required savings:

```
required_savings = sum(node.threshold * node.disruption_cost for all source nodes)
```

This sum-of-products approach ensures each NodePool's threshold applies only to its own pods. A critical workload's high threshold protects its pods without blocking consolidation of batch workloads that have lower requirements. See "Design Considerations" for alternatives considered.

**Configuration:**

```yaml
spec:
  disruption:
    consolidationSavingsThresholdCents: "1"  # cents/hr per disruption-unit
```

Default is 0 (existing behavior). Negative values are rejected at admission. For cluster-wide defaults, use a policy tool like Kyverno.

The units are cents/hr per disruption-unit: a threshold of 1 means "require 1 cent per hour savings for each pod-equivalent disrupted." The US has long discussed deprecating the penny; perhaps it can live on here in spirit.

**Issue #7146 with proposed algorithm (threshold = 1 cent/hr):**

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 1.0 + 5 * 1.0 = 6.0
Replacement: m7i-flex.large @ $0.080/hr
Savings: 0.6 cents/hr
Required: 1 * 6.0 = 6.0 cents/hr

Result: NO CONSOLIDATE (0.6 < 6.0)
```

The cascade is blocked at generation 1. Pods are not disrupted for six-tenths of a cent.

**Decision boundary: when does consolidation proceed?**

Same node, but consider replacing with m5.large @ $0.038/hr instead:

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 1.0 + 5 * 1.0 = 6.0
Replacement: m5.large @ $0.038/hr
Savings: 4.8 cents/hr
Required: 1 * 6.0 = 6.0 cents/hr

Result: NO CONSOLIDATE (4.8 < 6.0)
```

Still blocked. The decision flips when savings reaches 6.0 cents/hr:

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 6.0
Replacement: (hypothetical) @ $0.026/hr
Savings: 6.0 cents/hr
Required: 6.0 cents/hr

Result: CONSOLIDATE (6.0 >= 6.0)
```

The threshold creates a clear decision boundary: for this 5-pod node with threshold 1, consolidation requires at least 6 cents/hr savings.

## Design Considerations

### Why node_base_cost = 1.0?

Empty nodes currently have disruption cost = 0, making the savings-per-cost ratio undefined. We add a base cost of 1.0, equivalent to one default-priority pod.

We acknowledge 1.0 is not uniquely justified compared to 0.5 or 2.0. However, it is clearly better than extreme values (0.0 breaks the math; 10.0 would make empty nodes as hard to disrupt as nodes with critical workloads). The value captures operational overhead not tied to specific pods: connection draining, cache invalidation, API server load.

This primarily affects empty nodes (handled by the `WhenEmpty` consolidator) and lightly-loaded nodes. For nodes with many pods, the base cost is dominated by pod costs and has minimal effect.

We chose not to make this configurable because: (a) it's hard to reason about in isolation, (b) it only matters when pod disruption costs are small, and (c) users can achieve similar effects by adjusting their threshold. See Future Work if demand emerges.

### Why sum-of-products for multi-node consolidation?

Alternatives considered:

- **max(threshold)**: One critical node vetoes consolidation of batch nodes entirely
- **min(threshold)**: Batch workloads drag critical workloads into consolidation they didn't sign up for
- **Geometric mean**: Not applicable; we're summing costs, not averaging rates

Sum-of-products means each NodePool administrator sets policy for their pods. The total "price" of the consolidation is the sum of each stakeholder's price. No veto, no dragging.

**Multi-node example:**

```
Node A (pool-critical): $0.50/hr, 2 pods, threshold 5 cents/hr
  Disruption cost: 1.0 + 2 * 1.0 = 3.0
  Required savings from A: 5 * 3.0 = 15.0 cents/hr

Node B (pool-batch): $0.50/hr, 10 pods, threshold 1 cent/hr
  Disruption cost: 1.0 + 10 * 1.0 = 11.0
  Required savings from B: 1 * 11.0 = 11.0 cents/hr

Total required savings: 15.0 + 11.0 = 26.0 cents/hr
Replacement: $0.90/hr
Actual savings: $1.00 - $0.90 = 10.0 cents/hr

Result: NO CONSOLIDATE (10.0 < 26.0)
```

Node A contributes only $0.50/hr to total cost but dominates the required savings. This correctly reflects that pool-critical's strict disruption bar applies to its pods, even when consolidating alongside batch workloads.

At threshold 0 for pool-batch, its contribution would be 0 cents, so total required = 15 cents. Still NO CONSOLIDATE (10 < 15). The theoretical decision boundary for this consolidation is when savings reach 26 cents/hr (at current thresholds) or 15 cents/hr (if pool-batch uses threshold 0).

### Interaction with Spot-to-Spot Consolidation

The [Spot Consolidation RFC](spot-consolidation.md) requires 15 cheaper instance type options before single-node spot-to-spot consolidation proceeds. This RFC adds a savings threshold filter.

For spot-to-spot single-node consolidation:

1. Find instance types cheaper than current node
2. Filter to types where `savings >= required_savings`
3. Require 15+ remaining candidates (per spot-consolidation.md)
4. Send viable candidates to PCO for selection

The 15-candidate requirement applies to candidates that pass the savings threshold. If fewer than 15 candidates yield sufficient savings, no spot-to-spot consolidation occurs. See Future Work for potential refinements.

### Interaction with Other Disruption Controls

The threshold composes with NodePool budgets naturally. Budgets limit disruption concurrency and timing; the threshold gates whether individual consolidations are worth doing.

Existing filters run before the threshold check:
- **consolidateAfter**: Nodes must have the `Consolidatable` condition
- **ConsolidationPolicy**: Must be `WhenEmptyOrUnderutilized`
- **PodDisruptionBudgets**: Respected during scheduling simulation

These filters determine which nodes are candidates. The threshold determines whether a proposed consolidation yields sufficient savings.

### DaemonSet Pod Handling

DaemonSet pods are included in the disruption cost calculation but excluded from reschedulable pods. This is correct:

- **Included in cost**: Evicting a DaemonSet pod causes real disruption (graceful termination, potential service interruption during restart)
- **Excluded from rescheduling**: DaemonSet pods don't need explicit rescheduling; the DaemonSet controller automatically schedules them on the replacement node

The existing Karpenter code handles this correctly. This RFC does not change that behavior.

## Limitations

This RFC addresses the obvious failure mode: consolidating for trivial savings while disrupting many pods. It does not solve the general problem of optimal consolidation decisions.

The decision boundary remains fuzzy because we lack key information:

1. **Destination node lifetime**: We don't always know how long the replacement will survive. Moving pods to a node that gets interrupted 5 minutes later wastes the move cost.

2. **Workload termination probability**: A batch job at 95% completion might finish before we can amortize the move cost. We don't model this.

3. **Cascade effects**: Consolidating node A might enable or block consolidating node B. We evaluate greedily, not globally.

4. **Spot market dynamics**: Spot prices and availability change. A consolidation that looks marginal now might look great (or terrible) in an hour.

The threshold is a heuristic that blocks obviously wasteful consolidations. It will not find the globally optimal strategy.

## Configuration Guidance

**How to reason about threshold values:**

For a node with N default-priority pods (not near expiration), disruption cost is approximately N+1. At threshold T (cents/hr), required savings = T * (N+1) cents/hr.

| Threshold | 5-pod node requires | 15-pod node requires | Intuition |
|-----------|---------------------|----------------------|-----------|
| 1         | 6 cents/hr          | 16 cents/hr          | Block penny-shaving between similar instance types |
| 5         | 30 cents/hr         | 80 cents/hr          | Require savings like moving between smallest C/M/R sizes |
| 10        | 60 cents/hr         | 160 cents/hr         | Only consolidate for major size-class jumps |

The right value is somewhere in the 1-10 range for most clusters. The smallest price difference between EC2 M-family instance sizes is a reasonable anchor: if moving from m5.large to m5a.large (1 cent/hr) feels wasteful, set a threshold that blocks it.

**Recommended approach:**

1. Start with threshold = 0 (current behavior)
2. If you observe excessive churn without meaningful savings, increase threshold
3. Monitor the `karpenter_consolidation_threshold_blocked_total` metric
4. If blocked count rises but costs don't decrease, threshold may be too aggressive

Users experiencing churn should examine their consolidation events, estimate what threshold would have blocked the problematic ones, and move in that direction.

## Observability

**Logging:** Log blocked consolidations at info level (when threshold > 0):
```
consolidation blocked: savings 0.6 cents/hr below required 6.0 cents/hr (disruption_cost=6.0, threshold=1)
```

**Proposed metric:**
```
karpenter_consolidation_threshold_blocked_total{nodepool="default"}
```

Counter increments each time consolidation is rejected due to threshold. Operators can alert on this to detect overly aggressive thresholds.

## Implementation Scope

The threshold check integrates into the existing consolidation path:

**Modified packages:**
- `pkg/apis/v1/`: Add `ConsolidationSavingsThresholdCents` field to NodePool disruption spec
- `pkg/controllers/disruption/consolidation.go`: Add threshold check in `computeConsolidation()`
- `pkg/controllers/disruption/types.go`: Add node_base_cost to disruption cost calculation

**Key changes:**
- In `computeConsolidation()`, after computing candidate prices, calculate `required_savings = sum(threshold * disruption_cost)` across all source nodes
- Filter replacement options to those where `sum(candidatePrices) - replacementPrice >= required_savings`
- Add metric increment when consolidation is blocked

Estimated scope: ~100-150 lines including tests. No architectural changes; the threshold is an additional filter in the existing flow.

## Future Work

Several extensions are deferred to keep this RFC focused:

- **Configurable node_base_cost**: If demand emerges for tuning empty-node behavior independently
- **Destination lifetime consideration**: Factor in replacement node's remaining lifetime when evaluating savings
- **Adaptive threshold tuning**: AIMD-style adjustment based on consolidation outcomes (too much churn = increase threshold; costs stagnant = decrease)
- **Spot candidate count interaction**: Analyze whether the 15-candidate requirement should be relaxed when savings threshold is high
- **Learning from outcomes**: Use historical consolidation data to calibrate thresholds automatically
