# Consolidation Savings Threshold

## Problem Statement

Karpenter's consolidation logic creates excessive node churn by replacing nodes for marginal cost savings. Users report nodes being consolidated every 5-10 minutes, causing unnecessary pod disruption and reduced reliability. The workarounds are painful: disabling spot-to-spot consolidation, extensive PDB usage, or extended consolidateAfter periods.

**Motivating example ([aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)):** A production EKS cluster with ~15 nodes experiences daily cascading consolidation during off-peak hours. Nodes get marked "Underutilised" and disrupted; replacements get marked underutilized within 5-10 minutes; this cascades for 2-3 generations. Nodes at 95% CPU utilization are terminated. The cluster sometimes ends with *more* nodes than before. Pods restart up to 4 times in succession. The user sees no actual efficiency gained - node topologies are comparable before and after.

The root cause is that Karpenter consolidates whenever a cheaper alternative exists, without requiring meaningful improvement. Two nodes at $0.50/hr each can consolidate to one node at $0.95/hr - saving $0.05/hr while disrupting all workloads on both nodes.

This RFC builds on [spot-consolidation.md](spot-consolidation.md), which identified "Price Improvement Factor" as future work, and [PR #2562](https://github.com/kubernetes-sigs/karpenter/pull/2562), which proposed a percentage-based threshold. We adopt #2562's sound decisions: NodePool-level configuration, a default of 0 that preserves existing behavior, and string-based CRD fields to avoid float serialization issues. The key change is the formula: instead of a flat percentage, we normalize savings by disruption cost.

With a default threshold of 0, existing consolidation behavior is unchanged. Users opt in to the new behavior, and only marginal consolidations - those where savings don't justify the disruption - are blocked.

## Definitions

**Disruption cost** quantifies the relative difficulty of evicting pods from a node. It is a unitless heuristic used for ordering nodes from "easiest to disrupt" to "hardest to disrupt." The absolute value is not directly interpretable - it exists to enable consistent comparisons.

The disruption cost is computed as (see `pkg/utils/disruption/disruption.go` for the existing per-pod calculation):

```
per_pod_cost = clamp(1.0 + pod_priority/2^25 + pod_deletion_cost/2^27, -10.0, 10.0)
disruption_cost = node_base_cost + sum(per_pod_cost for all pods)
```

Where:
- **node_base_cost**: A fixed cost of 1.0 for disrupting the node itself, independent of pod count. This captures inherent disruption costs (connection draining, cache invalidation, API server load) and ensures empty nodes have non-zero cost. See "Design Decisions" below for rationale.
- **1.0**: Base cost per pod, ensuring pod count contributes to cost even without explicit priority or deletion cost
- **pod_priority**: From the pod's PriorityClass (default 0), divided by 2^25 (~33M) to normalize the int32 range to roughly [-64, +60]
- **pod_deletion_cost**: From the `controller.kubernetes.io/pod-deletion-cost` annotation (default 0), divided by 2^27 (~134M) for similar normalization
- **clamp**: Each pod's contribution is clamped to [-10, 10], preventing any single pod from dominating the cost

**Note on lifetime_remaining:** The existing Karpenter code multiplies disruption cost by `lifetime_remaining` (fraction of time until `expireAfter`). This RFC proposes removing that multiplier and using an up-front filter instead. See "Design Decisions" below.

This design has useful properties:
- An empty node has disruption cost = 1.0 (non-zero, so savings/cost is defined)
- A node with 10 default pods has disruption cost = 11.0 (node base + pod count)
- A node with 10 system-critical pods (priority 2B) has disruption cost = 101.0 (node base + each pod maxed at 10.0)

**Savings per disruption unit** is `savings / disruption_cost`. This ratio answers: "how much savings do we get per unit of disruption difficulty?" Higher values indicate more valuable consolidations.

The threshold has units of **$/hr per disruption-unit**. A disruption-unit approximates one default-priority pod. For example, a threshold of 0.01 means: "require at least $0.01/hr savings for each pod-equivalent being disrupted."

## Design Options

### 1. Savings per Disruption Unit [Recommended]

Normalize savings by the disruption cost of the consolidation:

For single-node consolidation:
```
required_savings = threshold * disruption_cost
consolidate when: savings >= required_savings
```

For multi-node consolidation, sum the required savings across all source nodes:
```
required_savings = sum(node.threshold * node.disruption_cost for all source nodes)
consolidate when: savings >= required_savings
```

This "sum-of-products" approach is more principled than taking the maximum threshold. Each node's threshold applies to its own disruption contribution, not to the entire consolidation. A critical workload's high threshold protects *its* pods without blocking consolidation of other workloads that have lower requirements.

The selection algorithm is: filter replacement options to those yielding sufficient savings, then select the cheapest. This separates concerns cleanly - the threshold gates "is this worth doing at all?" while cost optimization selects the best option among viable candidates.

Configuration at NodePool level:
```yaml
spec:
  disruption:
    minSavingsPerDisruptionUnit: "0.01"  # $/hr per disruption-unit
```

If not set, threshold defaults to 0 (existing behavior). For cluster-wide defaults, use a policy tool like Kyverno to set thresholds across NodePools. A threshold of 0 means any positive savings justifies consolidation.

**TODO:** Validate the threshold guidance table in the appendix against production workloads before finalizing recommendations.

See appendix for examples.

- :+1::+1::+1: Sum-of-products formula respects each NodePool's disruption bar independently
- :+1::+1: Threshold is consistent and interpretable: $/hr per disruption-unit
- :+1: Naturally scales with node size, pod count, and workload characteristics
- :+1: Default of 0 preserves existing behavior - opt-in adoption
- :-1: Disruption cost is a heuristic - users must calibrate empirically (see Configuration Guidance in appendix)

### 2. Price Improvement Percentage (PR #2562)

Flat percentage threshold for minimum price improvement:

```
consolidate when: replacement_price < current_price * (1 - threshold/100)
```

- :+1: Simple to understand ("require 20% savings")
- :-1: Same threshold for lightly-loaded and heavily-loaded nodes
- :-1: Arbitrary - why 20% vs 15% vs 25%?
- :-1: Doesn't answer "how would a customer set this?"
- :-1: Forces users to reason about nodes - Karpenter's value proposition is that users think about pods and constraints, not infrastructure

### 3. Extended ConsolidateAfter Period

Delay consolidation of recently launched nodes (e.g., 4 hours).

- :+1: No new configuration surface
- :-1: Still eventually converges to lowest price (just slower)
- :-1: Doesn't prevent marginal consolidation, just delays it

## Recommendation

Option 1 directly addresses the problem and answers the question @ellistarn raised in PR #2562: "how would a customer decide how much to configure this?" The savings-per-disruption-unit metric scales automatically with workload characteristics and defaults to existing behavior.

Option 2 could be layered on top of Option 1 in the future if users want a simpler "require X% savings" knob, but Option 1 solves the core problem.

## Design Decisions

### Why node_base_cost = 1.0?

The node base cost of 1.0 represents the inherent overhead of disrupting a node, independent of pod count. This value is analogous to the default per-pod eviction cost (also 1.0). It captures operational costs not tied to specific pods: connection draining, cache invalidation, API server load from the disruption event, etc.

This primarily affects empty nodes (handled by the `WhenEmpty` consolidator) and lightly-loaded nodes. For nodes with many pods, the node base cost is dominated by pod costs and has minimal effect on the threshold check.

We chose not to make this configurable because: (a) it's hard to reason about in isolation, (b) it only matters when pod disruption costs are small, and (c) users can achieve similar effects by adjusting their threshold. If demand emerges for tuning this, it could become a NodePool parameter in future work.

### Why remove the lifetime_remaining multiplier?

The existing Karpenter code computes disruption cost as:
```
disruption_cost = sum(per_pod_cost) * lifetime_remaining
```

This makes near-expiration nodes *easier* to consolidate (lower cost = higher savings-per-cost ratio). However, this inverts the economic intuition:

- If a node expires in 10 minutes and we consolidate now, total savings = hourly_savings * (10/60) = small
- The disruption (pod eviction, rescheduling) is the same whether we consolidate now or wait
- If we wait, the expiration controller handles the disruption anyway

For near-expiration nodes, the expected savings are so small that consolidation isn't worth the operational complexity. The pods will be disrupted soon regardless.

**This RFC proposes:** Remove the `lifetime_remaining` multiplier from disruption cost. Instead, add an up-front filter:

```
if lifetime_remaining < 0.1:  # <10% remaining
    skip candidate (let expiration handle it)
```

This matches the spot consolidation RFC's philosophy of using up-front filters (like the 15-candidate requirement) rather than continuous adjustments. It's also simpler to reason about.

**TODO:** Determine the right threshold for "near expiration." Options: 10% of expireAfter, fixed 15 minutes, or configurable. Need to consider interaction with consolidateAfter.

**Note:** This is a behavior change from existing Karpenter. The RFC should be explicit that we're modifying how disruption cost is computed.

## Interaction with Other Disruption Controls

The consolidation threshold check applies only to nodes that are already consolidation candidates. Existing filters run first:

- **consolidateAfter**: Nodes must have the `Consolidatable` condition (set after `consolidateAfter` elapses since last pod event)
- **ConsolidationPolicy**: Must be `WhenEmptyOrUnderutilized`
- **PodDisruptionBudgets**: Respected during scheduling simulation
- **Near-expiration filter** (new): Skip nodes with <10% lifetime remaining

These filters determine *which* nodes are candidates. The consolidation threshold determines *whether* a proposed consolidation yields sufficient savings. The two concerns compose cleanly.

## Interaction with Spot-to-Spot Consolidation

The [Spot Consolidation RFC](spot-consolidation.md) requires 15 cheaper instance type options before single-node spot-to-spot consolidation proceeds. This RFC adds a savings threshold filter.

The order of operations is:

1. Find all instance type candidates cheaper than current node(s)
2. For each candidate, compute potential savings = current_price - candidate_price
3. Filter to candidates where: `savings >= sum(threshold_i * disruption_cost_i)`
4. For single-node spot-to-spot: require 15+ viable candidates remaining
5. Select the cheapest among viable candidates

The 15-candidate requirement applies to candidates that pass the savings threshold. If fewer than 15 candidates yield sufficient savings, no spot-to-spot consolidation occurs.

**TODO:** Consider whether the 15-candidate requirement should be relaxed when a strong savings threshold is in place. For example, 7 candidates that all pass a meaningful threshold might be sufficient. Defer to future work.

## Appendix

### Behavior Examples

All scenarios assume pods with default priority (0) and no deletion cost annotation.
Per-pod cost = clamp(1.0 + 0 + 0, -10, 10) = 1.0. Node base cost = 1.0.

**Scenario 1: Marginal savings, light load**
```
Node: m5.large @ $0.096/hr, 3 pods
Disruption cost: 1.0 + 3 * 1.0 = 4.0
Replacement: m5a.large @ $0.086/hr (savings = $0.01/hr)
Required savings: 0.01 * 4.0 = $0.04/hr
Threshold: 0.01

Result: NO CONSOLIDATE (0.01 < 0.04)
```

**Scenario 2: Significant savings, light load**
```
Node: m5.xlarge @ $0.192/hr, 3 pods
Disruption cost: 1.0 + 3 * 1.0 = 4.0
Replacement: m5.large @ $0.096/hr (savings = $0.096/hr)
Required savings: 0.01 * 4.0 = $0.04/hr
Threshold: 0.01

Result: CONSOLIDATE (0.096 >= 0.04)
```

**Scenario 3: Significant savings, heavy load**
```
Node: m5.4xlarge @ $0.768/hr, 30 pods
Disruption cost: 1.0 + 30 * 1.0 = 31.0
Replacement: m5.2xlarge @ $0.384/hr (savings = $0.384/hr)
Required savings: 0.01 * 31.0 = $0.31/hr
Threshold: 0.01

Result: CONSOLIDATE (0.384 >= 0.31)
```

**Scenario 4: Near-expiration node (with proposed filter)**
```
Node: m5.large @ $0.096/hr, 3 pods, 5% lifetime remaining
Replacement: m5a.large @ $0.086/hr (savings = $0.01/hr)

Result: SKIP (below 10% lifetime threshold, let expiration handle)
```
The up-front filter prevents consolidation of nodes that will expire soon anyway.

**Scenario 5: Empty node**
```
Node: m5.large @ $0.096/hr, 0 pods
Disruption cost: 1.0 (node base cost only)
Replacement: m5a.large @ $0.086/hr (savings = $0.01/hr)
Required savings: 0.01 * 1.0 = $0.01/hr
Threshold: 0.01

Result: CONSOLIDATE (0.01 >= 0.01)
```
Empty nodes still have non-zero disruption cost due to node base cost.

**Scenario 6: Node with negative-priority pod (batch job)**
```
Node: m5.large @ $0.096/hr, 5 pods total
  - 4 default pods: 4 * 1.0 = 4.0
  - 1 batch pod (priority -1M): 1.0 + (-1M/2^25) = 1.0 - 0.03 = 0.97
Disruption cost: 1.0 + 4.0 + 0.97 = 5.97
```
Negative priority makes nodes slightly easier to disrupt.

**Scenario 7: Multi-node consolidation with sum-of-products**
```
Node A (pool-critical): $0.50/hr, 2 pods, threshold 0.05
  Disruption cost: 1.0 + 2 * 1.0 = 3.0
  Required savings from A: 0.05 * 3.0 = $0.15/hr

Node B (pool-batch): $0.50/hr, 10 pods, threshold 0.005
  Disruption cost: 1.0 + 10 * 1.0 = 11.0
  Required savings from B: 0.005 * 11.0 = $0.055/hr

Total required savings: $0.15 + $0.055 = $0.205/hr
Replacement: $0.90/hr
Actual savings: $1.00 - $0.90 = $0.10/hr

Result: NO CONSOLIDATE (0.10 < 0.205)
```
Each NodePool's threshold applies to its own disruption contribution.

**Scenario 8: Multi-node with zero-cost node**
```
Node A: $0.50/hr, 0 pods, 0% lifetime remaining (filtered out)
Node B: $0.50/hr, 10 pods, 100% lifetime remaining

Action: Node A is filtered by near-expiration check.
        Only Node B is considered for consolidation.
```

**Scenario 9: High-disruption node blocks cheap consolidation**
```
Node A (pool-critical): $0.10/hr, 2 pods, threshold 0.10
  Disruption cost: 3.0
  Required: 0.10 * 3.0 = $0.30/hr

Node B (pool-batch): $0.90/hr, 50 pods, threshold 0.001
  Disruption cost: 51.0
  Required: 0.001 * 51.0 = $0.051/hr

Total required: $0.351/hr
Replacement: $0.95/hr
Actual savings: $0.05/hr

Result: NO CONSOLIDATE (0.05 < 0.351)
```
Node A contributes only $0.10/hr to cost but dominates the threshold requirement. This correctly reflects that pool-critical's strict disruption bar applies to its pods.

### Configuration Guidance

**TODO:** Validate these values against production workloads before finalizing.

| Threshold | Behavior | When to Use |
|-----------|----------|-------------|
| 0 (default) | Consolidate for any savings | Current behavior, maximum cost optimization |
| 0.001 | Aggressive | Stateless, fast-starting workloads |
| 0.01 | Moderate | General-purpose, mixed workloads |
| 0.10 | Conservative | Slow-starting, stateful, cache-heavy workloads |

**How to reason about the threshold:** For a node with 10 default pods, disruption cost is 11.0 (1.0 node base + 10 pods). A threshold of 0.01 means: "require at least $0.11/hr savings to justify disrupting this node" (11.0 * 0.01 = $0.11/hr minimum savings).

Unlike a percentage threshold, this naturally scales: a node with more pods or higher-priority pods requires more savings to justify disruption.

### Observability

Operators need visibility into consolidation decisions, both accepted and rejected.

**Log every decision** with savings and threshold:
```
# Blocked:
consolidation blocked: savings $0.01/hr below required $0.04/hr (disruption_cost=4.0, threshold=0.01)

# Accepted:
consolidation proceeding: savings $0.096/hr meets required $0.04/hr (disruption_cost=4.0, threshold=0.01)
```

**Proposed metric:**
```
karpenter_consolidation_threshold_blocked_total{nodepool="default"}
```

This counter increments each time a consolidation is rejected due to the threshold. Labeled by the source candidate's NodePool. Operators can alert on this metric to detect if their threshold is too aggressive.

**TODO:** Consider whether histogram or gauge metrics for savings/required ratio would justify the cardinality cost.
