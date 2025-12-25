# Consolidation Savings Threshold

## Problem Statement

The current consolidation algorithm fires whenever a cheaper alternative exists:

```
consolidate when: replacement_price < current_price
```

This causes excessive churn when savings are marginal. Users report cascading consolidation where nodes are replaced, then replacements are replaced within minutes, disrupting pods multiple times for negligible benefit.

**Case study: [aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)**

A cluster with 14-17 nodes experienced cascading consolidation during off-peak hours:

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

Less than a penny per hour. The user disrupted 5 pods, waited for graceful termination, rescheduled workloads, and then did it again 10 minutes later. The disruption cost dwarfed any savings. The real cost to users depends on how each pod disruption impacts their application--lost requests, increased latency, cache invalidation, or SLA breaches.

The workarounds are painful: disabling spot-to-spot consolidation entirely, extensive PDB usage, or multi-hour `consolidateAfter` periods. Users shouldn't have to choose between "consolidation works" and "consolidation doesn't destroy my cluster."

This RFC builds on [spot-consolidation.md](spot-consolidation.md) from [Ellis Tarn](https://github.com/ellistarn), which identified "Price Improvement Factor" as future work, and [RFC 2562](https://github.com/kubernetes-sigs/karpenter/pull/2562) from [Jukie](https://github.com/jukie), which moved that conversation forward.

## Definitions and Context

Karpenter computes a **disruption cost** for each node to order consolidation candidates from easiest to hardest to disrupt (see Appendix C for the full formula). For practical purposes: a node with N default-priority pods has disruption cost approximately N. Higher-priority pods and pods with deletion-cost annotations increase the cost; nodes approaching expiration have reduced cost.

**Key assumption:** We reuse disruption cost for economic thresholding. This is a pragmatic approximation--the heuristic was designed for ordering, not absolute measurement. However, it already encodes what we care about (pod count, priority, deletion cost, remaining lifetime) and is battle-tested in production. Using the same signal for thresholding is simpler than inventing a parallel metric.

**Savings threshold formula:** Given a threshold T (dollars per hour per unit of disruption cost), the required savings for consolidation is:

```
required_savings = T * disruption_cost
```

For a node with N default-priority pods not near expiration, disruption cost is approximately N, so required savings is approximately T * N.

**EC2 price analysis (December 2025, us-east-1):** To calibrate the threshold, we examined price differences between EC2 instance types:

| Instance Type | vCPU | Memory (GiB) | On-Demand Price ($/hr) |
|---------------|------|--------------|------------------------|
| m8i.large     | 2    | 8            | $0.1058                |
| c8i.xlarge    | 4    | 8            | $0.1874                |
| m8i.xlarge    | 4    | 16           | $0.2117                |
| r8i.xlarge    | 4    | 32           | $0.2778                |
| m8i.2xlarge   | 8    | 32           | $0.4234                |

Cross-family differences at xlarge: m8i to c8i saves $0.024/hr; r8i to m8i saves $0.066/hr. A threshold of 0.01 means a 5-pod node requires $0.05/hr savings to consolidate. This blocks m8i-to-c8i churn ($0.024 < $0.05) while allowing r8i-to-m8i moves ($0.066 >= $0.05) when capacity permits.

## Design Options

### 1. Disruption-Normalized Threshold [Recommended]

Require savings to exceed a threshold scaled by disruption cost.

**Motivating scenario:** A spot node running 8 pods could consolidate to 50 cheaper instance types. Of those, 12 save only $0.02-$0.05/hr while 38 save $0.08/hr or more. Should we include the marginal-savings options in our candidate pool, or filter them out?

This approach filters to the 38 candidates that meet a per-pod savings bar. With 8 pods and threshold 0.01, require $0.08/hr savings.

**Modified algorithm:**

```
consolidate when: savings >= required_savings
```

**Issue #7146 with proposed algorithm (threshold = 0.01):**

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 5.0
Replacement: m7i-flex.large @ $0.080/hr
Savings: $0.006/hr
Required: 0.01 * 5.0 = $0.05/hr

Result: NO CONSOLIDATE ($0.006 < $0.05)
```

The cascade is blocked at generation 1. Pods are not disrupted for less than a penny. For this 5-pod node with threshold 0.01, consolidation requires at least $0.05/hr savings--the decision flips when a cheaper replacement (hypothetical, since such a node may not exist) crosses that boundary.

**Multi-node consolidation:**

When consolidating multiple nodes to one replacement, sum the disruption costs:

```
Node A: $0.50/hr, 3 pods -> disruption cost = 3.0
Node B: $0.50/hr, 7 pods -> disruption cost = 7.0
Total disruption cost: 10.0
Required savings: 0.01 * 10.0 = $0.10/hr

Replacement: $0.90/hr
Actual savings: $1.00 - $0.90 = $0.10/hr

Result: CONSOLIDATE ($0.10 >= $0.10)
```

The 7-pod node contributes more to the required savings than the 3-pod node, correctly reflecting that it's more disruptive to move.

* 👍👍👍 Incorporates linear pod-level disruption cost: to save $0.10/hr, disrupting 5 pods passes (requires $0.05) while disrupting 50 pods fails (requires $0.50). The binary gate comes from the sort.
* 👍👍 Filters candidates using primitives from existing code; the threshold distinguishes high-value from low-value moves
* 👍 Composes cleanly with NodePool budgets and existing disruption controls
* 👎 Introduces a parameter that users must reason about
* 👎 The "right" threshold value is workload-dependent

### 2. Flat Percentage Threshold

Require replacement to be X% cheaper than current node (e.g., "only consolidate if replacement is 20% cheaper").

**Motivating scenario with 20% threshold:**

```
Source: m5.xlarge spot @ $0.07/hr, 8 pods
Required savings: 20% of $0.07 = $0.014/hr

Candidate pool (50 instance types cheaper than $0.07/hr):
  - 12 types save $0.02-$0.05/hr  -> all pass (>= $0.014)
  - 38 types save $0.08-$0.30/hr  -> all pass

Candidates passing threshold: 50 (all of them)
```

The marginal-savings candidates aren't filtered. To exclude candidates saving $0.05/hr, we'd need a 71% threshold--absurdly high.

**Pod-count blindness:** The same 8-pod node and a 50-pod node at $0.07/hr have identical thresholds. Disrupting 50 pods for $0.02/hr savings is treated the same as disrupting 8 pods.

This approach is intuitive ("require 20% savings") but percentages interact poorly with spot's already-low prices, and pod count is ignored entirely.

* 👍👍 Easy to explain: "require 20% savings"
* 👍 Familiar pattern from other systems
* 👎 Ignores pod-level disruption cost: disrupting 50 pods is treated the same as disrupting 5 pods
* 👎 Fixed percentage interacts poorly with discrete instance pricing
* 👎 Hard to pick a value that works across node sizes

### 3. Minimum Absolute Savings

Require a fixed dollar amount (e.g., "only consolidate if we save $0.10/hr").

**Motivating scenario with $0.10/hr threshold:**

```
Source: m5.xlarge spot @ $0.07/hr, 8 pods
Required savings: $0.10/hr (fixed)

Candidate pool (50 instance types cheaper than $0.07/hr):
  - 12 types save $0.02-$0.05/hr  -> filtered out (< $0.10)
  - 18 types save $0.08-$0.09/hr  -> filtered out (< $0.10)
  - 20 types save $0.10-$0.30/hr  -> pass

Candidates passing threshold: 20
```

This filters more aggressively than Option 2, but the threshold is arbitrary. Why $0.10? The "right" value depends on how many pods you're disrupting, but this approach can't express that.

**Pod-count blindness:** A node with 2 pods and a node with 50 pods both require $0.10/hr to consolidate. Saving $0.10/hr while disrupting 2 pods is reasonable; saving $0.10/hr while disrupting 50 pods is wasteful.

Simple and predictable. Works for homogeneous clusters. Fails in heterogeneous environments: a small threshold allows churn on high-pod nodes; a large threshold blocks legitimate consolidation of smaller nodes.

* 👍👍 Dead simple to configure
* 👍 Predictable behavior
* 👎 Ignores pod-level disruption cost: disrupting 50 pods is treated the same as disrupting 5 pods
* 👎 Single value cannot suit both small and large nodes

### 4. Time-Based Dampener

Prevent consolidation of recently-launched nodes (e.g., "don't consolidate nodes younger than 4 hours").

**Motivating scenario with 4-hour dampener:**

```
Source: m5.xlarge spot @ $0.07/hr, 8 pods, launched 2 hours ago

Candidate pool (50 instance types cheaper than $0.07/hr):
  - 12 types save $0.02-$0.05/hr
  - 38 types save $0.08-$0.30/hr

Result: NO CONSOLIDATE (node age 2hr < 4hr threshold)
```

Two hours later, the node hits the 4-hour mark:

```
Source: m5.xlarge spot @ $0.07/hr, 8 pods, launched 4 hours ago

Result: PROCEED with all 50 candidates (including marginal-savings options)
```

The dampener delays consolidation but doesn't filter candidates. Once the time passes, all 50 candidates--including the 12 marginal-savings options--are back in play. The destructive cascade (pods restarted 4 times, nodes replaced then re-replaced for pennies) simply starts 4 hours later.

**Pod-count blindness:** A 2-pod node and a 50-pod node launched at the same time are treated identically.

This prevents bad moves by preventing all moves. A node saving $2/hr is blocked just as effectively as one saving $0.006/hr. Time-based controls have uses (see spot-consolidation.md on minimum node lifetime), but they don't distinguish high-value from low-value opportunities.

* 👍 Simple to implement
* 👍 Directly addresses "too much churn" symptom
* 👎 Ignores pod-level disruption cost: disrupting 50 pods is treated the same as disrupting 5 pods
* 👎 Blocks good consolidations alongside bad ones
* 👎 Doesn't scale with savings magnitude
* 👎 Interacts awkwardly with consolidateAfter

## Recommendation

Option 1 (Disruption-Normalized Threshold) addresses the core problem: savings should justify disruption. Options 2-4 ignore pod-level disruption cost entirely and could layer on top of Option 1 for defense in depth; we defer layering until we learn from feedback.

**Configuration:** Expose a NodePool-level `consolidationSavingsThreshold` field. The default value is an open question for community input--see "Consolidate-Delete Behavior" below. Users who want savings-based filtering set a non-zero threshold (e.g., `consolidationSavingsThreshold: 0.01`).

**How to choose a threshold:** The threshold is dollars per hour per unit of disruption cost. For nodes with default-priority pods, disruption cost approximates pod count, so:

| Threshold | 5-pod node requires | 10-pod node requires | 50-pod node requires |
|-----------|---------------------|----------------------|----------------------|
| 0.005     | $0.025/hr           | $0.05/hr             | $0.25/hr             |
| 0.01      | $0.05/hr            | $0.10/hr             | $0.50/hr             |
| 0.02      | $0.10/hr            | $0.20/hr             | $1.00/hr             |

Start with 0.01. If consolidation is too aggressive (still seeing cascades), increase to 0.02. If consolidation is too conservative (missing obvious savings), decrease to 0.005.

**Interactions:** The threshold filter applies before spot's 15-candidate flexibility check (see Appendix A). Existing disruption controls (consolidateAfter, ConsolidationPolicy, PDBs) determine candidacy; the threshold gates whether a candidate yields sufficient savings.

**Consolidate-Delete Behavior (needs community input):** For consolidate-delete (terminating a node without launching a replacement), savings equal the full source node price. This can block consolidation when disruption cost is high relative to node price:

```
Node: $0.10/hr, 20 pods, threshold 0.01
Required savings: 0.01 * 20 = $0.20/hr
Actual savings: $0.10/hr (the full node price)

Result: NO CONSOLIDATE ($0.10 < $0.20)
```

Is this correct? We believe yes: the threshold says "to disrupt 20 pods, require $0.20/hr savings." The node only saves $0.10/hr--the disruption cost outweighs the savings. This is intentional. Customers who want this consolidation can configure their pods (lower priority, deletion-cost annotations) or NodePools to reduce disruption cost. The disruption cost would need to decrease by 2x or more for consolidation to proceed.

This represents a behavioral change from today, where consolidate-delete always proceeds if capacity exists elsewhere. We believe this is the right direction--"free money" isn't free if it costs 20 pod disruptions--but the community should weigh in. The default threshold value is particularly important: a non-zero default (e.g., 0.01) enables this behavior by default; a zero default preserves current behavior.

**Performance:** O(1) arithmetic per node using already-computed disruption costs; no measurable impact expected.

**Observability:** Log blocked consolidations at info level with savings, required savings, disruption cost, and threshold. Add counter metric `karpenter_consolidation_threshold_blocked_total{nodepool}` for alerting. See Appendix B.

**Rollback:** Set threshold to 0 to restore previous behavior.

## Appendix A: Spot Consolidation Example (SPOT PRICES)

**All prices in this appendix are SPOT prices**, typically 60-70% lower than on-demand. The on-demand prices in the Context section are for calibration only.

For spot-to-spot single-node consolidation:

1. Find instance types cheaper than current node
2. Filter to types where `savings >= required_savings` for this source node
3. Require 15+ remaining candidates (per spot-consolidation.md)
4. Send viable candidates to PCO for selection

**Example:**

```
Source: m5.xlarge spot @ $0.07/hr, 8 pods, threshold 0.01
  Disruption cost: 8.0
  Required savings: 0.01 * 8.0 = $0.08/hr

Candidate pool (50 instance types cheaper than $0.07/hr):
  - 12 types save $0.02-$0.05/hr  -> filtered out (below $0.08/hr required savings)
  - 18 types save $0.08-$0.15/hr  -> pass
  - 20 types save $0.15-$0.30/hr  -> pass

Candidates passing threshold: 38
Required minimum: 15

Result: PROCEED with 38 candidates sent to PCO
```

The 15-candidate requirement applies to candidates that pass the savings threshold. If fewer than 15 candidates yield sufficient savings, no spot-to-spot consolidation occurs.

## Appendix B: Implementation Details

**Modified packages:**
- `pkg/controllers/disruption/consolidation.go`: Add threshold check in `computeConsolidation()`

**Key changes:**
- In `computeConsolidation()`, after computing candidate prices, calculate `required_savings = threshold * disruption_cost` for all source nodes
- When launching a replacement node, filter candidates to those where `sum(source_node_prices) - replacement_price >= required_savings`. For consolidate-delete (no replacement), savings equal the full source price
- Add metric increment when consolidation is blocked

**Logging format:**
```
consolidation blocked: savings $0.006/hr below required $0.05/hr (disruption_cost=5.0, threshold=0.01)
```

**Metric (tentative):**
```
karpenter_consolidation_threshold_blocked_total{nodepool="default"}
```

Estimated scope: ~100-150 lines including tests. No architectural changes; the threshold is an additional filter in the existing flow.

## Appendix C: Disruption Cost Formula

The full disruption cost formula (see `pkg/utils/disruption/disruption.go`):

```
disruption_cost = sum(per_pod_cost) * lifetime_remaining
```

Where `per_pod_cost = clamp(1.0 + pod_priority/2^25 + pod_deletion_cost/2^27, -10.0, 10.0)`.

- The base cost of 1.0 ensures pod count contributes even without explicit priority
- Priority and deletion-cost annotations are normalized to avoid any single pod dominating
- The `lifetime_remaining` multiplier (fraction of time until `expireAfter`) reduces cost as nodes approach expiration

Example values (assuming `lifetime_remaining = 1.0`):
- Empty node: 0.0
- Node with 10 default-priority pods: 10.0
- Node with 10 system-cluster-critical pods: 100.0 (each pod clamped to max 10.0)
