# Consolidation Savings Threshold

## Problem Statement

Karpenter's consolidation logic creates excessive node churn by replacing nodes for marginal cost savings. Users report nodes being consolidated every 5-10 minutes, causing unnecessary pod disruption and reduced reliability. The workarounds are painful: disabling spot-to-spot consolidation, extensive PDB usage, or extended consolidateAfter periods.

**Motivating example ([aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)):** A production EKS cluster with ~15 nodes experiences daily cascading consolidation during off-peak hours. Nodes get marked "Underutilised" and disrupted; replacements get marked underutilized within 5-10 minutes; this cascades for 2-3 generations. Nodes with 95% of vCPU allocated are terminated because Karpenter finds marginally cheaper alternatives. The cluster ends with *more* nodes than before. Pods restart up to 4 times in succession. The user sees no actual efficiency gained - node topologies are comparable before and after. We call these "marginal consolidations" because the disruption doesn't justify the payoff.

The root cause is that Karpenter consolidates whenever a cheaper alternative exists, without requiring meaningful improvement. Two nodes at $0.50/hr each can consolidate to one node at $0.95/hr - saving $0.05/hr while disrupting all workloads on both nodes.

This RFC builds on [spot-consolidation.md](spot-consolidation.md), which identified "Price Improvement Factor" as future work, and [PR #2562](https://github.com/kubernetes-sigs/karpenter/pull/2562), which proposed a percentage-based threshold. We adopt #2562's sound decisions: NodePool-level configuration, a default of 0 that preserves existing behavior, and string-based CRD fields to avoid float serialization issues. The key change is the formula: instead of a flat percentage, we normalize savings by disruption cost.

With a default threshold of 0, existing consolidation behavior is unchanged. Users opt in to the new behavior, and only marginal consolidations - those where savings don't justify the disruption - are blocked.

## Definitions

**Disruption cost** quantifies the relative difficulty of evicting pods from a node. It is a unitless heuristic used for ordering nodes from "easiest to disrupt" to "hardest to disrupt." The absolute value is not directly interpretable - it exists to enable consistent comparisons.

Karpenter already computes a per-pod disruption cost (see `pkg/utils/disruption/disruption.go`). This RFC proposes using that existing metric for a new purpose: gating consolidation decisions. The per-pod formula is:

```
per_pod_cost = clamp(1.0 + pod_priority/2^25 + pod_deletion_cost/2^27, -10.0, 10.0)
```

Where:
- **1.0**: Base cost per pod, ensuring pod count contributes to cost even without explicit priority or deletion cost
- **pod_priority**: From the pod's PriorityClass (default 0), divided by 2^25 (~33M) to normalize the int32 range to roughly [-64, +60]
- **pod_deletion_cost**: From the `controller.kubernetes.io/pod-deletion-cost` annotation (default 0), divided by 2^27 (~134M) for similar normalization
- **clamp**: Each pod's contribution is clamped to [-10, 10], preventing any single pod from dominating the cost

This RFC proposes adding a **node_base_cost** of 1.0 to the total disruption cost. Combined with the existing `lifetime_remaining` multiplier (see "Interaction with lifetime_remaining" below), the complete formula is:

```
disruption_cost = (node_base_cost + sum(per_pod_cost for all pods)) * lifetime_remaining
```

The node base cost ensures empty nodes have non-zero disruption cost, capturing inherent overhead (connection draining, cache invalidation, API server load) independent of pod count. See "Design Decisions" below for rationale.

This design has useful properties (assuming `lifetime_remaining = 1.0`):
- An empty node has disruption cost = 1.0 (non-zero, so savings/cost is defined)
- A node with 10 default pods has disruption cost = 11.0 (node base + pod count)
- A node with 10 system-cluster-critical pods (priority ~2 billion) has disruption cost = 101.0 (node base + each pod maxed at 10.0)
- A node at 50% of its expireAfter lifetime has these values halved

**Savings per disruption unit** is `savings / disruption_cost`. This ratio answers: "how much savings do we get per unit of disruption difficulty?" Higher values indicate more valuable consolidations.

The threshold has units of **$/hr per disruption-unit**. A disruption-unit approximates one default-priority pod. For example, a threshold of 0.01 means: "require at least $0.01/hr savings for each pod-equivalent being disrupted."

## Design Options

### 1. Savings per Disruption Unit [Recommended]

Normalize savings by the disruption cost of the consolidation:

For single-node consolidation:
```
required_savings = threshold * disruption_cost
consolidate when: savings > 0 AND savings >= required_savings
```

The `savings > 0` check ensures we never consolidate when there's no actual cost reduction, even with threshold = 0.

For multi-node consolidation, sum the required savings across all source nodes:
```
required_savings = sum(node.threshold * node.disruption_cost for all source nodes)
consolidate when: savings > 0 AND savings >= required_savings
```

This "sum-of-products" approach is more principled than taking the maximum threshold. Each node's threshold applies to its own disruption contribution, not to the entire consolidation. A critical workload's high threshold protects *its* pods without blocking consolidation of other workloads that have lower requirements.

The selection algorithm is: filter replacement options to those yielding sufficient savings, then select the cheapest. This separates concerns cleanly - the threshold gates "is this worth doing at all?" while cost optimization selects the best option among viable candidates.

Configuration at NodePool level:
```yaml
spec:
  disruption:
    consolidationSavingsThreshold: "0.01"  # $/hr per disruption-unit
```

If not set, threshold defaults to 0 (existing behavior). For cluster-wide defaults, use a policy tool like Kyverno to set thresholds across NodePools. A threshold of 0 means any positive savings justifies consolidation. Negative thresholds are invalid and rejected at admission.

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

The existing Karpenter code computes disruption cost as the sum of per-pod costs, with no node-level component. This means empty nodes have disruption cost = 0, which makes the savings-per-cost ratio undefined.

We propose adding a node base cost of 1.0 to ensure empty nodes have non-zero disruption cost. This value is analogous to the default per-pod eviction cost (also 1.0), though we acknowledge 1.0 is not uniquely justified compared to 0.5 or 2.0. However, it is clearly better than extreme values (0.0, 0.001, 10, or 100) and provides a reasonable starting point.

The node base cost captures operational overhead not tied to specific pods: connection draining, cache invalidation, API server load from the disruption event. This primarily affects empty nodes (handled by the `WhenEmpty` consolidator) and lightly-loaded nodes. For nodes with many pods, the node base cost is dominated by pod costs and has minimal effect on the threshold check.

We chose not to make this configurable because: (a) it's hard to reason about in isolation, (b) it only matters when pod disruption costs are small, and (c) users can achieve similar effects by adjusting their threshold. If demand emerges for tuning this, it could become a NodePool parameter in future work.

### Interaction with lifetime_remaining

The existing Karpenter code multiplies disruption cost by `lifetime_remaining` (fraction of time until `expireAfter`):

```go
DisruptionCost: disruptionutils.ReschedulingCost(ctx, pods) * disruptionutils.LifetimeRemaining(clk, nodePool, node.NodeClaim)
```

This makes near-expiration nodes easier to disrupt (lower cost), prioritizing them for consolidation. This behavior is also used by the static deprovisioning controller (`pkg/controllers/static/deprovisioning/controller.go`) to choose which nodes to terminate when scaling down.

This RFC does not propose changing the `lifetime_remaining` multiplier. The existing behavior remains: near-expiration nodes have lower disruption cost and are prioritized for consolidation. The savings threshold check uses this existing disruption cost as-is.

## Interaction with Other Disruption Controls

The consolidation threshold check applies only to nodes that are already consolidation candidates. Existing filters run first:

- **consolidateAfter**: Nodes must have the `Consolidatable` condition (set after `consolidateAfter` elapses since last pod event)
- **ConsolidationPolicy**: Must be `WhenEmptyOrUnderutilized`
- **PodDisruptionBudgets**: Respected during scheduling simulation

These filters determine *which* nodes are candidates. The consolidation threshold determines *whether* a proposed consolidation yields sufficient savings. The two concerns compose cleanly.

## Interaction with Spot-to-Spot Consolidation

The [Spot Consolidation RFC](spot-consolidation.md) requires 15 cheaper instance type options before single-node spot-to-spot consolidation proceeds. This RFC adds a savings threshold filter.

For spot-to-spot single-node consolidation:

1. Find instance types cheaper than current node (savings > 0)
2. Filter to candidates where: `savings >= threshold * disruption_cost`
3. Require 15+ remaining candidates (per spot-consolidation.md)
4. Send viable candidates to PCO for selection

The 15-candidate requirement applies to candidates that pass the savings threshold. If fewer than 15 candidates yield sufficient savings, no spot-to-spot consolidation occurs.

## Failure Modes

The threshold is a sensitivity/specificity tradeoff:

- **Threshold too low (approaching 0):** Current behavior - consolidation fires for marginal savings. Excessive churn persists. Symptom: you observe churn in your cluster AND the proposed blocked-consolidation metric (see Observability) is always zero.

- **Threshold too high:** Consolidation rarely fires. Nodes remain underutilized longer than necessary. Monitor for rising blocked count or stagnant node costs.

- **Threshold of 0:** Equivalent to current behavior. Safe default for users who don't want to change anything.

- **Misconfigured threshold:** No crashes or errors, just suboptimal consolidation behavior. Safe to tune in production. Start with default (0), observe consolidation patterns, increase threshold if churn is excessive.

## Appendix

### Behavior Examples

All scenarios assume:
- Pods with default priority (0) and no deletion cost annotation: per-pod cost = 1.0
- Node base cost = 1.0
- `lifetime_remaining = 1.0` (nodes not near expiration)

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

**Scenario 4: Empty node**
```
Node: m5.large @ $0.096/hr, 0 pods
Disruption cost: 1.0 (node base cost only)
Replacement: m5a.large @ $0.086/hr (savings = $0.01/hr)
Required savings: 0.01 * 1.0 = $0.01/hr
Threshold: 0.01

Result: CONSOLIDATE (0.01 >= 0.01)
```
Empty nodes still have non-zero disruption cost due to node base cost.

**Scenario 5: Node with negative-priority pod (batch job)**
```
Node: m5.large @ $0.096/hr, 5 pods total
  - 4 default pods: 4 * 1.0 = 4.0
  - 1 batch pod (priority -1M): 1.0 + (-1M/2^25) = 1.0 - 0.03 = 0.97
Disruption cost: 1.0 + 4.0 + 0.97 = 5.97
Replacement: m5a.large @ $0.086/hr (savings = $0.01/hr)
Required savings: 0.01 * 5.97 = $0.0597/hr
Threshold: 0.01

Result: NO CONSOLIDATE (0.01 < 0.0597)
```
Negative priority makes nodes slightly easier to disrupt, but doesn't dramatically change the threshold check.

**Scenario 6: Multi-node consolidation with sum-of-products**
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

**Scenario 7: High-disruption node blocks cheap consolidation**
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

**Scenario 8: Successful multi-node consolidation**
```
Node A (pool-default): $0.50/hr, 5 pods, threshold 0.01
  Disruption cost: 1.0 + 5 * 1.0 = 6.0
  Required savings from A: 0.01 * 6.0 = $0.06/hr

Node B (pool-default): $0.50/hr, 5 pods, threshold 0.01
  Disruption cost: 1.0 + 5 * 1.0 = 6.0
  Required savings from B: 0.01 * 6.0 = $0.06/hr

Total required savings: $0.06 + $0.06 = $0.12/hr
Replacement: $0.60/hr
Actual savings: $1.00 - $0.60 = $0.40/hr

Result: CONSOLIDATE (0.40 >= 0.12)
```
With uniform thresholds and sufficient savings, multi-node consolidation proceeds.

### Configuration Guidance

**How to reason about the threshold:** For a node with N default-priority pods (not near expiration), disruption cost is approximately N+1 (1.0 node base + N pods at 1.0 each). At threshold 0.01, this means:

- 10-pod node: requires $0.11/hr minimum savings (11.0 * 0.01)
- 30-pod node: requires $0.31/hr minimum savings (31.0 * 0.01)
- Empty node: requires $0.01/hr minimum savings (1.0 * 0.01)

Users should start with the default (0) and increase the threshold if they observe excessive marginal consolidations in their metrics. Unlike a percentage threshold, this naturally scales: a node with more pods or higher-priority pods requires more savings to justify disruption.

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
