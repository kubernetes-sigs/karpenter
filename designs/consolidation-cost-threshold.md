# Consolidation Savings Threshold

## Problem Statement

Karpenter consolidation removes nodes from the cluster when doing so improves efficiency. Empty nodes are easy, but consolidating non-empty nodes causes disruptions to running pods.

The current consolidation algorithm fires whenever a cheaper alternative exists (`replacement_price < current_price`). This causes excessive churn when savings are marginal.

**Case study: [aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)**

A cluster with 14-17 nodes experienced cascading consolidation during off-peak hours: 7 nodes disrupted over 30 minutes, replacement nodes re-disrupted within 5-10 minutes, pods restarted up to 4 times. The algorithm saw:

```
Node: m6a.large @ $0.086/hr, ~5 pods
Replacement: m7i-flex.large @ $0.080/hr
Savings: $0.006/hr -> CONSOLIDATE
```

Current workarounds (disabling spot-to-spot consolidation, extensive PDBs, multi-hour `consolidateAfter`) are painful.

This RFC builds on [spot-consolidation.md](spot-consolidation.md) (Ellis Tarn) and [RFC 2562](https://github.com/kubernetes-sigs/karpenter/pull/2562) (Jukie, open).

## Context

Karpenter computes a **disruption cost** for each node to order consolidation candidates. For nodes with default-priority pods, disruption cost approximates pod count. The `lifetime_remaining` multiplier reduces cost as nodes approach expiration - this means nodes naturally become easier to consolidate as they age, so disrupting a node with 10 minutes left costs less than disrupting one with 10 hours left.

```
disruption_cost = sum(per_pod_cost) * lifetime_remaining

per_pod_cost = clamp(1.0 + pod_priority/2**25 + pod_deletion_cost/2**27, -10.0, 10.0)
```

This is existing Karpenter behavior; we could change it for this RFC, but don't see a compelling reason to do so. See `pkg/utils/disruption/disruption.go` for details.

**Savings threshold:** Given a threshold T (dollars per hour per unit of disruption cost):

```
required_savings = T * disruption_cost
```

For a 5-pod node with threshold 0.01, required savings is $0.05/hr. We calibrated 0.01 against the minimum price step between instance types when sorted by cost in us-east-1 (see Appendix A). The absolute prices matter less than the relative gaps: a threshold that blocks cross-family churn ($0.024/hr difference) while allowing meaningful savings ($0.066/hr) should generalize across regions and providers with similar instance type diversity.

**Why linear?** We use a linear relationship because it's the simplest monotonic function. Disrupting 10 pods requires 10x the savings of disrupting 1 pod. If production feedback suggests diminishing returns (log) or compounding effects (quadratic), we can revisit.

## Design Options

### 1. Disruption-Normalized Threshold [Recommended]

Require savings to exceed a threshold scaled by disruption cost.

**Motivating scenario:** A spot node running 8 pods could consolidate to 50 cheaper instance types. Of those, 12 save only $0.02-$0.05/hr while 38 save $0.08/hr or more. Should we include the marginal-savings options in our candidate pool, or filter them out?

This approach filters to the 38 candidates that meet a per-pod savings bar. With 8 pods and threshold 0.01, require $0.08/hr savings.

**Modified algorithm:**

During candidate evaluation, Karpenter filters instance types where `savings < required_savings`. Remaining candidates proceed through the existing selection logic:

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

The cascade is blocked at generation 1. Pods are not disrupted for less than a penny. For this 5-pod node with threshold 0.01, consolidation requires at least $0.05/hr savings. (If the node were 90% through its lifetime, disruption cost drops to 0.5, required savings to $0.005/hr, and consolidation proceeds.)

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

The 7-pod node contributes more to the required savings than the 3-pod node, correctly reflecting that it's more disruptive to move. (If these nodes were 50% through their lifetime, disruption cost drops to 5.0 and required savings to $0.05/hr - consolidation becomes easier as nodes age.)

* 👍👍👍 Incorporates linear pod-level disruption cost: to save $0.10/hr, disrupting 5 pods passes (requires $0.05) while disrupting 50 pods fails (requires $0.50).
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

Option 1 (Disruption-Normalized Threshold) addresses the core problem: savings should justify disruption. Options 2-4 can layer for defense in depth; we defer layering until we learn from feedback.

For example, layering a 10% percentage floor would block consolidation options that pass the disruption threshold but save less than 10% in relative terms. Some users might want this; we're open to it.

> **Seeking feedback:** This RFC proposes alpha with default 0 (preserving existing behavior). We plan a follow-up RFC to change the default to 0.01 after gathering production feedback. We would like community input on: (1) if 0.01 is an appropriate value, or if we should pick something else, (2) the consolidate-delete behavior for cheap nodes with a lot of pods (we believe this is correct - cheap dense nodes *should* be harder to delete - but want confirmation), and (3) interaction with spot consolidation's 15-candidate minimum.

**API:** NodePool-level `consolidationSavingsThreshold` field (dollars per hour per unit of disruption cost; approximately: dollars per hour per pod for default-priority workloads). The threshold is a NodePool-level setting because it governs node-level consolidation decisions. Pod-level disruption preferences are expressed through existing mechanisms: pod priority, PodDisruptionBudgets, and the `karpenter.sh/do-not-disrupt` annotation. Most users should not need to touch the default. See Appendix B for tuning guidance.

**Lifecycle:** Starts as alpha, annotated with `// Note: This field is alpha.` in code. Default value of 0 preserves existing behavior (opt-in). Graduates to stable after production feedback.

**Spot interaction:** The threshold filter applies before spot's 15-candidate flexibility check (see spot-consolidation.md). If threshold filtering leaves fewer than 15 candidates, no spot-to-spot consolidation occurs. Remaining candidates are sent to Price Capacity Optimized (PCO) allocation for selection. This is intentional: if no replacement offers sufficient savings, don't consolidate.

**Where this fits in the consolidation pipeline:**

```
CANDIDATE ELIGIBILITY (before decision generation)
  |
  +-- NodePool static checks, label validation
  +-- ConsolidateAfter / ConsolidationPolicy
  +-- Consolidatable condition
  +-- Not in disruption queue
  +-- Node/Pod disruption validation (PDBs, do-not-disrupt)
  +-- Disruption budget available
  |
  v
SCHEDULING SIMULATION
  |
  +-- All pods can schedule without candidate
  +-- At most 1 new node required
  |
  v
PRICING FILTERS (after scheduling simulation)
  |
  +-- Order instance types by price
  +-- Filter to types < current price
  +-- [THIS RFC] Filter to types meeting savings threshold  <--
  +-- For spot single-node: require 15+ candidates
  +-- Cap to 15 types for launch
  |
  v
VALIDATION (re-check before execution)
  |
  +-- Re-simulate scheduling
  +-- Re-check candidate eligibility
  +-- Disruption budget still available
```

The savings threshold filter slots in after removing more-expensive options but before spot's flexibility check.

**Consolidate-delete behavior:** For consolidate-delete (terminating without replacement), savings equal the full node price. A $0.10/hr node with 20 pods requires $0.20/hr savings (threshold 0.01) - consolidation is blocked:

```
Node: $0.10/hr, 20 pods -> required savings: 0.01 * 20 = $0.20/hr
Savings: $0.10/hr (full node price) -> NO CONSOLIDATE
```

At 10 pods on this same node, required savings drops to $0.10/hr and consolidate-delete proceeds. The threshold creates a crossover point where cheap dense nodes stay put while cheap sparse nodes consolidate.

We recommend this behavior: the threshold says "to disrupt 20 pods, require $0.20/hr savings." This represents a behavioral change.

**Default threshold:** We recommend 0.01 as the target value but will default to 0 for alpha to preserve existing behavior. Assuming positive production feedback, we will submit a follow-up RFC to change the default to 0.01.

**Performance:** O(1) arithmetic per node; no measurable impact.

**Observability:** Log blocked consolidations at info level. Add counter `karpenter_consolidation_threshold_blocked_total{nodepool}`.

**Rollback:** Set threshold to 0.

**Out of scope:**

- Destination node lifetime filtering for consolidate-delete (pods moving to a node with 30 minutes remaining will be disrupted again soon)
- Break-even time model (require savings to pay off within N hours rather than rate-based threshold)
- Pod-lifetime-aware disruption costs (pods running longer aren't weighted higher)
- Non-linear scaling (logarithmic, quadratic)
- Per-namespace or per-workload thresholds

These could be explored in future RFCs if production feedback indicates need.

## Appendix A: Examples and Price Calibration

For spot-to-spot single-node consolidation:

1. Find instance types cheaper than current node
2. Filter to types where `savings >= required_savings` for this source node
3. Require 15+ remaining candidates (per spot-consolidation.md)
4. Send viable candidates to PCO for selection

**Spot example:**

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

(If this node were 50% through its lifetime, disruption cost drops to 4.0, required savings to $0.04/hr, and all 50 candidates pass.)

**On-demand price calibration (December 2025, us-east-1):**

| Instance Type | vCPU | Memory (GiB) | On-Demand Price ($/hr) |
|---------------|------|--------------|------------------------|
| m8i.large     | 2    | 8            | 0.1058                 |
| c8i.xlarge    | 4    | 8            | 0.1874                 |
| m8i.xlarge    | 4    | 16           | 0.2117                 |
| r8i.xlarge    | 4    | 32           | 0.2778                 |
| m8i.2xlarge   | 8    | 32           | 0.4234                 |

Cross-family differences at xlarge: m8i to c8i saves $0.024/hr; r8i to m8i saves $0.066/hr. A threshold of 0.01 means a 5-pod node requires $0.05/hr savings to consolidate. This blocks m8i-to-c8i churn ($0.024 < $0.05) while allowing r8i-to-m8i moves ($0.066 >= $0.05) when capacity permits.

Users who want to allow m8i-to-c8i moves can lower the threshold or reduce pod priorities. Users who want to block r8i-to-m8i moves can raise the threshold or increase pod priorities.

## Appendix B: Tuning Guide

The threshold is dollars per hour per unit of disruption cost. For nodes with default-priority pods:

| Threshold | 5-pod node requires | 10-pod node requires | 50-pod node requires |
|-----------|---------------------|----------------------|----------------------|
| 0.005     | $0.025/hr           | $0.05/hr             | $0.25/hr             |
| 0.01      | $0.05/hr            | $0.10/hr             | $0.50/hr             |
| 0.02      | $0.10/hr            | $0.20/hr             | $1.00/hr             |

Start with the default.

**When to increase:** Nodes are consolidating multiple times per day for small savings (<$0.10/hr each). Pods are being disrupted more than workload owners expect.

**When to decrease:** Obvious cost savings opportunities aren't being taken. Nodes run for hours at low utilization without consolidating.
