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

Less than a penny per hour. The user disrupted 5 pods, waited for graceful termination, rescheduled workloads, and then did it again 10 minutes later. The disruption cost dwarfed any savings.

The workarounds are painful: disabling spot-to-spot consolidation entirely, extensive PDB usage, or multi-hour `consolidateAfter` periods. Users shouldn't have to choose between "consolidation works" and "consolidation doesn't destroy my cluster."

This RFC builds on [spot-consolidation.md](spot-consolidation.md), which identified "Price Improvement Factor" as future work.

## Definitions and Context

Karpenter computes a **disruption cost** for each node to order consolidation candidates from easiest to hardest to disrupt. The formula (see `pkg/utils/disruption/disruption.go`):

```
disruption_cost = sum(per_pod_cost) * lifetime_remaining
```

Where `per_pod_cost = clamp(1.0 + pod_priority/2^25 + pod_deletion_cost/2^27, -10.0, 10.0)`. The base cost of 1.0 ensures pod count contributes even without explicit priority. Priority and deletion-cost annotations are normalized to avoid any single pod dominating the total. The `lifetime_remaining` multiplier (fraction of time until `expireAfter`) reduces effective cost as nodes approach expiration.

This formula produces a unitless heuristic for ordering: higher cost means harder to disrupt. Example values (assuming `lifetime_remaining = 1.0`):
- Empty node: 0.0
- Node with 10 default-priority pods: 10.0
- Node with 10 system-cluster-critical pods: 100.0 (each pod clamped to max 10.0)

**Key assumption:** We treat disruption cost as a reasonable estimate for how costly it is to migrate pods off a node. The heuristic already encodes what we care about: pod count, priority, deletion cost, and remaining lifetime. Using the same signal to gate economic decisions is natural. The ordering says "disrupt this one before that one"; the threshold says "don't bother disrupting any of these." Same underlying question, different decision boundary.

**EC2 price analysis:** To calibrate the threshold, we examined price differences between EC2 instance types:

| Instance Type | vCPU | Memory (GiB) | On-Demand Price ($/hr) |
|---------------|------|--------------|------------------------|
| m8i.large     | 2    | 8            | $0.1058                |
| c8i.xlarge    | 4    | 8            | $0.1874                |
| m8i.xlarge    | 4    | 16           | $0.2117                |
| r8i.xlarge    | 4    | 32           | $0.2778                |
| m8i.2xlarge   | 8    | 32           | $0.4234                |

Cross-family differences at xlarge: m8i to c8i saves $0.024/hr; r8i to m8i saves $0.066/hr. A threshold of 0.01 means a 5-pod node requires $0.05/hr savings to consolidate. This blocks m8i-to-c8i churn ($0.024 < $0.05) while allowing r8i-to-m8i moves ($0.066 >= $0.05) when capacity permits.

## Design Options

**Motivating scenario:** A spot node running 8 pods could consolidate to 50 cheaper instance types. Of those, 12 save only $0.02-$0.05/hr while 38 save $0.08/hr or more. Should we include the marginal-savings options in our candidate pool, or filter them out?

- **Option 1 (Disruption-Normalized)**: Filter to the 38 candidates that meet a per-pod savings bar. With 8 pods, require $0.08/hr savings.
- **Option 2 (Flat Percentage)**: Keep or reject all 50 based on percentage of current price. Pod count doesn't matter.
- **Option 3 (Absolute Savings)**: Keep or reject all 50 based on a fixed dollar threshold. Pod count doesn't matter.
- **Option 4 (Time-Based)**: Don't filter by savings at all. Block consolidation if the node is too young.

### 1. Disruption-Normalized Threshold [Recommended]

Require savings to exceed a threshold scaled by disruption cost. The threshold represents dollars-per-hour savings required for each unit of disruption cost.

**Modified algorithm:**

```
required_savings = threshold * disruption_cost
consolidate when: savings > 0 AND savings >= required_savings
```

The `savings > 0` check ensures we never consolidate when there's no cost reduction, even with threshold = 0.

For a node with N default-priority pods (not near expiration), disruption cost is approximately N. At threshold T, required savings = T * N.

**Issue #7146 with proposed algorithm (threshold = 0.01):**

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 5.0
Replacement: m7i-flex.large @ $0.080/hr
Savings: $0.006/hr
Required: 0.01 * 5.0 = $0.05/hr

Result: NO CONSOLIDATE ($0.006 < $0.05)
```

The cascade is blocked at generation 1. Pods are not disrupted for less than a penny.

**Decision boundary: when does consolidation proceed?**

The decision flips when savings reaches $0.05/hr:

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 5.0
Replacement: (hypothetical, assuming we can find such a node) @ $0.036/hr
Savings: $0.05/hr
Required: 0.01 * 5.0 = $0.05/hr

Result: CONSOLIDATE ($0.05 >= $0.05)
```

The threshold creates a clear decision boundary: for this 5-pod node with threshold 0.01, consolidation requires at least $0.05/hr savings.

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

* 👍👍👍 Incorporates linear pod-level disruption cost: if we can save $0.10/hr by disrupting either 50 pods or 5 pods (all else equal), we prefer disrupting fewer pods. We can calculate exactly when pod count tips a "yes" to a "no."
* 👍👍 Orders consolidation using primitives from existing code; the threshold distinguishes high-value from low-value moves
* 👍 Composes cleanly with NodePool budgets and existing disruption controls
* 👎 Introduces a parameter that users must reason about
* 👎 The "right" threshold value is workload-dependent

### 2. Flat Percentage Threshold

Require replacement to be X% cheaper than current node (e.g., "only consolidate if replacement is 20% cheaper").

This approach is intuitive: "I want at least 20% savings to justify disruption." However, instance pricing follows discrete size steps, not continuous curves. A fixed percentage may block all consolidation for small nodes while allowing wasteful churn on large nodes. More fundamentally, it ignores pod count entirely. Disrupting one pod isn't free, and disrupting ten pods is even less free.

* 👍👍 Easy to explain: "require 20% savings"
* 👍 Familiar pattern from other systems
* 👎 Ignores pod-level disruption cost: disrupting 50 pods is treated the same as disrupting 5 pods
* 👎 Fixed percentage interacts poorly with discrete instance pricing
* 👎 Hard to pick a value that works across node sizes

### 3. Minimum Absolute Savings

Require a fixed dollar amount (e.g., "only consolidate if we save $0.10/hr").

Simple and predictable. Works well for homogeneous clusters. Fails in heterogeneous environments: a small threshold allows churn on high-pod nodes; a large threshold blocks legitimate consolidation of smaller nodes.

* 👍👍 Dead simple to configure
* 👍 Predictable behavior
* 👎 Ignores pod-level disruption cost: disrupting 50 pods is treated the same as disrupting 5 pods
* 👎 Single value cannot suit both small and large nodes

### 4. Time-Based Dampener

Prevent consolidation of recently-launched nodes (e.g., "don't consolidate nodes younger than 4 hours").

This prevents bad moves by preventing all moves. A node saving $2/hr is blocked just as effectively as one saving $0.006/hr. Time-based controls have uses (see spot-consolidation.md on minimum node lifetime), but they don't distinguish high-value from low-value opportunities.

* 👍 Simple to implement
* 👍 Directly addresses "too much churn" symptom
* 👎 Ignores pod-level disruption cost: disrupting 50 pods is treated the same as disrupting 5 pods
* 👎 Blocks good consolidations alongside bad ones
* 👎 Doesn't scale with savings magnitude
* 👎 Interacts awkwardly with consolidateAfter

## Recommendation

Option 1 (Disruption-Normalized Threshold) addresses the core problem: savings should justify disruption. Options 2-4 ignore pod-level disruption cost entirely. Options 2-4 could layer on top of Option 1 for defense in depth; we defer this layering until we learn from feedback.

**Configuration:** We expose a NodePool-level `consolidationSavingsThreshold` field with a default of 0.0 (current behavior preserved). Users who want savings-based filtering set `consolidationSavingsThreshold: 0.01`. We collect feedback and consider flipping the default in a future release once we understand real-world impact. If the threshold is too high, we block all consolidation operations. It might be sensible to fail with an error message when creating a NodePool that doesn't allow consolidation with this parameter, because a customer with that use case almost certainly wants `WhenEmpty` consolidation, and this is a cheap but more-expensive-than-necessary way to get that.

**Cloud provider variance:** The mechanism is vendor-neutral. Our EC2 analysis shows cross-family instance types typically differ by $0.02-0.07/hr; we expect similar patterns on GCP and Azure.

**Interactions:** The threshold filter applies before spot's 15-candidate flexibility check (see Appendix A). Existing disruption controls (consolidateAfter, ConsolidationPolicy, PDBs) determine candidacy; the threshold gates whether a candidate yields sufficient savings. DaemonSet pods contribute to disruption cost but don't require explicit rescheduling. For consolidate-delete (terminating a node without launching a replacement), savings equal the full source node price, so it will rarely be blocked by the threshold.

**Limitations:** This RFC addresses the obvious failure mode: consolidating for trivial savings while disrupting many pods. Algorithms like this are not correct in binary terms; they are incrementally less wrong over time. We lack information about destination node lifetime, workload termination probability, cascade effects, and spot market dynamics. The threshold blocks obviously wasteful consolidations but will not find globally optimal strategies. Future work includes node base costs, destination lifetime modeling, adaptive threshold tuning, and learning from outcomes.

**Performance:** The threshold check adds O(1) arithmetic per node using already-computed disruption costs. Our analysis assumes a cold controller start; in practice, we can benefit from reusing values for unchanged nodes. This is an RFC, so we don't know exactly how much latency we'll add, but it should be minimal unless we screw up the implementation.

**Observability:** Log blocked consolidations at info level with savings, required savings, disruption cost, and threshold. Add a counter metric `karpenter_consolidation_threshold_blocked_total{nodepool}` for alerting on overly aggressive thresholds. See Appendix B for details.

## Appendix A: Spot Consolidation Example

Note: This example uses spot prices, which are typically 60-70% lower than on-demand prices shown in the Context section.

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
- When launching a replacement node, filter candidates to those where `sum(candidate_source_prices) - replacement_destination_price >= required_savings`. For consolidate-delete (no replacement), savings equal the full source price
- Add metric increment when consolidation is blocked

**Logging format:**
```
consolidation blocked: savings $0.006/hr below required $0.05/hr (disruption_cost=5.0, threshold=0.01)
```

**Metric:**
```
karpenter_consolidation_threshold_blocked_total{nodepool="default"}
```

Estimated scope: ~100-150 lines including tests. No architectural changes; the threshold is an additional filter in the existing flow.
