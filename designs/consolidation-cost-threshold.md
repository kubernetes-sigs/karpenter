# Consolidation Savings Threshold

## Problem

Karpenter consolidation removes nodes from the cluster when doing so improves efficiency. Empty nodes are easy, but consolidating non-empty nodes causes disruptions to running pods. The current consolidation algorithm consolidates whenever a cheaper alternative exists (`replacement_price < current_price`). This causes excessive churn when savings are marginal.

**Case study:** [aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)

A cluster with 14-17 nodes experienced cascading consolidation during off-peak hours: 7 nodes disrupted over 30 minutes, replacement nodes re-disrupted within 5-10 minutes, some pods restarted up to 4 times. The algorithm saw:

```
Node: m6a.large @ $0.086/hr, ~5 pods
Replacement: m7i-flex.large @ $0.080/hr
Savings: $0.006/hr -> CONSOLIDATE
```

The community has proposed workarounds: disable spot-to-spot consolidation, add extensive PDBs, set multi-hour `consolidateAfter` delays. All feel like band-aids. None address the root cause: Karpenter consolidates for any positive savings, ignoring the cost of disruption.

Karpenter already computes a **disruption cost** for each node to order consolidation candidates. This RFC proposes using this cost as a gate: consolidate only when savings exceed a threshold proportional to disruption cost.

**Guiding principles:**

1. **Reuse existing calculations.** Karpenter already computes disruption cost and lifetime remaining. Build on these rather than inventing redundant mechanisms.
2. **Target the root cause, not the symptoms.** The root cause is that consolidation initiates moves without considering whether savings justify disruption. The solution should help any customer facing churn from marginal consolidation decisions.
3. **Give customers clearly interpretable parameters.** The solution should be as simple as possible, avoiding unnecessary complexity.

## Background: Disruption Cost in Karpenter

Today this cost is used only to *order* consolidation candidates (prefer disrupting lower-cost nodes first). This RFC proposes *also* using it as a gate: require savings to exceed a threshold proportional to disruption cost.

**The existing disruption cost formula:**

```
disruption_cost = sum(per_pod_cost) * lifetime_remaining
per_pod_cost = clamp(1.0 + pod_priority/2**25 + pod_deletion_cost/2**27, -10.0, 10.0)
```

Where:
- `pod_priority` is the Kubernetes pod priority (default 0, system-critical ~2 billion). The divisor 2^25 (~33 million) normalizes priority into a small contribution to per-pod cost.
- `pod_deletion_cost` is from the `controller.kubernetes.io/pod-deletion-cost` annotation (default 0). The divisor 2^27 (~134 million) similarly normalizes this value.
- For most workloads: default-priority pods contribute ~1.0 each to the sum.

The `lifetime_remaining` multiplier ranges from 1.0 (node just launched) to 0.0 (node at expiration). This progressively favors consolidation over stability as nodes age. A node 90% through its lifetime has a 0.1 multiplier, reducing required savings by 10x.

See `pkg/utils/disruption/disruption.go` for implementation details.

**Proposed savings threshold:**

Given a threshold T (dollars per hour per unit of disruption cost):
```
required_savings = T * disruption_cost
consolidate when: savings >= required_savings
```

**Related work:**
- [spot-consolidation.md](spot-consolidation.md): Introduced the 15-candidate flexibility floor for spot consolidation. This RFC complements that work.
- [RFC 2562](https://github.com/kubernetes-sigs/karpenter/pull/2562): Proposes a price improvement factor. This RFC takes a different approach (threshold scaled by disruption cost rather than fixed percentage), but the goals overlap. If both ship, they could layer; this RFC is the recommended starting point.

## Design Options

### 1. Disruption-Normalized Threshold [Recommended]

Require savings to exceed a threshold scaled by disruption cost. With 8 pods and threshold 0.01, require $0.08/hr savings to consolidate. This filters out marginal-savings candidates while allowing meaningful consolidation. See Appendix A for worked examples.

**Why 0.01?** The threshold is calibrated against typical cluster economics. The value means "require ~1 cent per hour per pod to justify disruption":

| Pod count | Required savings | Example blocked | Example allowed |
|-----------|------------------|-----------------|-----------------|
| 5 pods    | $0.05/hr         | $0.006/hr (m6a.large -> m7i-flex.large) | $0.07/hr (r8i.xlarge -> m8i.xlarge) |
| 10 pods   | $0.10/hr         | $0.02/hr (m8i.xlarge -> c8i.xlarge) | $0.21/hr (m8i.2xlarge -> m8i.xlarge) |
| 20 pods   | $0.20/hr         | $0.07/hr (r8i.xlarge -> m8i.xlarge) | $0.42/hr (m8i.2xlarge -> m8i.xlarge) |

Note: r8i -> m8i ($0.07/hr) is allowed for 5 pods but blocked for 20 pods. This is intended: more pods require more savings to justify disruption.

This blocks the $0.006/hr-for-5-pods move from issue #7146 while allowing meaningful consolidation. The value is conservative: high enough to prevent marginal-savings churn, low enough that legitimate consolidation proceeds.

**Why linear?** A linear relationship is the simplest monotonic function. Disrupting 10 pods should require 10x the savings of disrupting 1 pod. If production feedback suggests otherwise--diminishing returns (logarithmic) or compounding effects (quadratic)--we can revisit.

**Issue #7146 with proposed algorithm (threshold = 0.01):**

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 5.0
Replacement: m7i-flex.large @ $0.080/hr
Savings: $0.006/hr
Required: 0.01 * 5.0 = $0.05/hr

Result: NO CONSOLIDATE ($0.006 < $0.05)
```

The cascade is blocked at generation 1. For this 5-pod node with threshold 0.01, consolidation requires at least $0.05/hr savings. If the node were 90% through its lifetime, disruption cost drops to 0.5, required savings to $0.005/hr, and consolidation proceeds.

**Multi-node consolidation:**

When consolidating multiple nodes to one replacement, sum the disruption costs. Use sum rather than max because all pods are disrupted in a single operation--the total disruption is the sum of individual disruptions, not the worst single node. If max were used, consolidating 10 single-pod nodes would require only $0.01/hr savings, same as consolidating 1 single-pod node, despite disrupting 10x as many pods.

```
Node A: $0.50/hr, 3 pods -> disruption cost = 3.0
Node B: $0.50/hr, 7 pods -> disruption cost = 7.0
Total disruption cost: 10.0
Required savings: 0.01 * 10.0 = $0.10/hr

Replacement: $0.90/hr
Actual savings: $1.00 - $0.90 = $0.10/hr

Result: CONSOLIDATE ($0.10 >= $0.10)
```

The 7-pod node contributes more to required savings than the 3-pod node. If these nodes were 50% through their lifetime, disruption cost drops to 5.0 and required savings to $0.05/hr.

* 👍👍👍 Incorporates pod-level disruption cost into the decision: to save $0.10/hr, disrupting 5 pods passes (requires $0.05) while disrupting 50 pods fails (requires $0.50)
* 👍👍 Filters candidates using primitives from existing code; the threshold distinguishes high-value from low-value moves
* 👍 Composes cleanly with NodePool budgets and existing disruption controls
* 👎 Introduces a parameter that users must reason about
* 👎 The right threshold value is workload-dependent

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

The marginal-savings candidates aren't filtered. To exclude candidates saving $0.05/hr, we'd need a 71% threshold, which is absurdly high.

**Pod-count blindness:** An 8-pod node and a 50-pod node at $0.07/hr have identical thresholds. Disrupting 50 pods for $0.02/hr savings is treated the same as disrupting 8 pods.

This approach is intuitive ("require 20% savings") but fixed percentages interact poorly with spot's low prices. Pod count is ignored entirely.

* 👍👍 Easy to explain: "require 20% savings"
* 👍 Familiar pattern from other systems
* 👎 Ignores pod-level disruption cost: disrupting 50 pods is treated the same as disrupting 5 pods
* 👎 Fixed percentage interacts poorly with discrete instance pricing
* 👎 Hard to pick a value that works across node sizes

### 3. Minimum Absolute Savings

Require a fixed dollar amount (e.g., "only consolidate if savings exceed $0.10/hr").

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

**Pod-count blindness:** A 2-pod node and a 50-pod node both require $0.10/hr to consolidate. Saving $0.10/hr while disrupting 2 pods is reasonable; disrupting 50 pods for the same savings is wasteful.

This is simple and predictable. It works for homogeneous clusters but fails in heterogeneous environments: a small threshold allows churn on high-pod-count nodes, while a large threshold blocks legitimate consolidation of smaller nodes.

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

The dampener delays consolidation but doesn't filter candidates. Once the time threshold passes, all 50 candidates--including the 12 marginal-savings options--are back in play. The destructive cascade (pods restarted 4 times, nodes replaced then re-replaced for pennies) simply starts 4 hours later.

**Pod-count blindness:** A 2-pod node and a 50-pod node launched at the same time are treated identically.

This prevents bad moves by preventing all moves. A node saving $2/hr is blocked just as effectively as one saving $0.006/hr. Time-based controls have uses (see spot-consolidation.md on minimum node lifetime), but they cannot distinguish high-value from low-value opportunities.

* 👍 Simple to implement
* 👍 Directly addresses "too much churn" symptom
* 👎 Ignores pod-level disruption cost: disrupting 50 pods is treated the same as disrupting 5 pods
* 👎 Blocks good consolidations alongside bad ones
* 👎 Doesn't scale with savings magnitude
* 👎 Interacts awkwardly with consolidateAfter

## Recommendation

Option 1 (Disruption-Normalized Threshold) addresses the core problem: savings should justify disruption. Options 2-4 can layer for defense in depth; layering is deferred until production feedback is available.

For example, layering a 10% percentage floor would block consolidation options that pass the disruption threshold but save less than 10% in relative terms. Some users might want this behavior.

### Behavioral Change

This RFC changes default consolidation behavior. Today, any positive savings triggers consolidation. With this RFC, savings must exceed a threshold proportional to disruption cost.

This is the right default because:
1. The current behavior causes real harm (issue #7146: pods restarted 4 times for pennies)
2. The "any savings" assumption ignores the non-zero cost of disruption
3. Users who prefer legacy behavior can set threshold to 0

This is a more significant change than spot-consolidation.md's 15-candidate floor. Where that RFC added restrictions, this RFC changes the decision function. Expect questions from users whose consolidation behavior changes.

### API

NodePool-level `consolidationSavingsThreshold` field (dollars per hour per unit of disruption cost; approximately: dollars per hour per pod for default-priority workloads):

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationSavingsThreshold: "0.01"  # default; require ~$0.01/hr per pod
    # consolidationSavingsThreshold: "0"   # legacy behavior: any savings
    # consolidationSavingsThreshold: "0.02" # stricter: require ~$0.02/hr per pod
```

The threshold is a NodePool-level setting because it governs node-level consolidation decisions. Pod-level disruption preferences are expressed through existing mechanisms: pod priority, PodDisruptionBudgets, and the `karpenter.sh/do-not-disrupt` annotation. Most clusters should not need to change the default. See Appendix B for tuning guidance.

### Lifecycle

Starts as alpha, annotated with `// Note: This field is alpha.` in code. Users who want legacy behavior can set threshold to 0. Graduates to stable after production feedback.

### Spot Interaction

The threshold filter applies before spot's 15-candidate flexibility check (see spot-consolidation.md). If threshold filtering leaves fewer than 15 spot candidates, no spot-to-spot consolidation occurs. Remaining candidates are sent to Price Capacity Optimized (PCO) allocation for selection.

This interaction is intentional. The two mechanisms are complementary:
- Savings threshold: "don't consolidate unless savings justify disruption"
- 15-candidate minimum: "don't consolidate spot unless there's flexibility to avoid racing to the bottom"

**Edge case:** When threshold filtering leaves fewer than 15 spot candidates:

```
Source: cheap spot node, 8 pods, threshold 0.01
Required savings: $0.08/hr

50 cheaper candidates -> 36 filtered (< $0.08/hr) -> 14 remain (< 15 required)

Result: NO CONSOLIDATE
```

Both conditions are doing their job: the threshold filters low-value moves, the 15-candidate minimum ensures sufficient flexibility for spot. If this blocks more consolidation than desired, lower the threshold.

### Pipeline Position

The savings threshold filter slots in after removing more-expensive options but before the spot flexibility check:

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
  +-- [THIS RFC] Filter by savings threshold
  +-- For spot single-node: require 15+ candidates
  +-- Cap to cheapest 15 types for launch
  |
  v
VALIDATION (re-check before execution)
  |
  +-- Re-simulate scheduling
  +-- Re-check candidate eligibility
  +-- Disruption budget still available
```

Example: An 8-pod spot node with threshold 0.01 requires $0.08/hr savings. Starting with 100 instance types, 50 are cheaper than current. The threshold filter removes 12 marginal-savings candidates, leaving 38. Since 38 >= 15, spot consolidation proceeds; the cheapest 15 are sent to PCO.

### Empty Nodes

For nodes with zero pods, disruption cost is 0, so required savings is 0. Empty nodes always consolidate, assuming other eligibility checks pass. This is correct: there's nothing to disrupt.

```
Node: $0.10/hr, 0 pods
  Disruption cost: 0
  Required savings: 0.01 * 0 = $0.00/hr
  Savings: $0.10/hr -> DELETE PROCEEDS
```

### Consolidate-Delete Behavior

For consolidate-delete (terminating without replacement), savings equal the full node price. A $0.10/hr node with 20 pods requires $0.20/hr savings (threshold 0.01), so consolidation is blocked:

```
Node: $0.10/hr, 20 pods, freshly launched (lifetime_remaining = 1.0)
  Disruption cost: 20.0 * 1.0 = 20.0
  Required savings: 0.01 * 20.0 = $0.20/hr
  Savings: $0.10/hr (full node price) -> NO DELETE
```

The threshold creates a crossover point where cheap dense nodes stay put while sparse nodes consolidate. The lifetime_remaining multiplier provides an escape valve: as nodes age, they become deletable regardless of pod count.

```
Same node, 90% through its lifetime (lifetime_remaining = 0.1)
  Disruption cost: 20.0 * 0.1 = 2.0
  Required savings: 0.01 * 2.0 = $0.02/hr
  Savings: $0.10/hr -> DELETE PROCEEDS
```

This behavior is intentional. Disruption cost is either meaningful or it is not. If not, the disruption cost formula (used throughout Karpenter) should be fixed. If meaningful, then the threshold correctly applies a consistent standard: "to disrupt 20 pods, require savings proportional to 20 pods' worth of disruption."

Pod restarts have real costs: connection drains, cache warming, state reconstruction, brief unavailability. The disruption cost formula attempts to capture these costs. Users who believe their pods move "for free" can lower the threshold.

### Operational Concerns

**Performance:** The threshold comparison is O(1) per candidate. Filtering candidates by threshold is O(candidates). This adds negligible overhead to the existing consolidation pipeline.

**Observability:** Log blocked consolidations at info level. Add metric `karpenter_consolidation_threshold_blocked_total{nodepool}`.

**Rollback:** Set threshold to 0.

### Out of Scope

- Destination node lifetime filtering for consolidate-delete (pods moving to a node with 30 minutes remaining will be disrupted again soon)
- Break-even time model (require savings to pay back within N hours rather than rate-based threshold)
- Pod-lifetime-aware disruption costs (weight long-running pods higher)
- Non-linear scaling (logarithmic, quadratic)
- Per-namespace or per-workload thresholds

These could be explored in future RFCs if production feedback indicates a need.

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

If this node were 50% through its lifetime, disruption cost drops to 4.0, required savings to $0.04/hr, and all 50 candidates pass.

**On-demand price calibration (December 2025, us-east-1):**

| Instance Type | vCPU | Memory (GiB) | On-Demand Price ($/hr) |
|---------------|------|--------------|------------------------|
| m8i.large     | 2    | 8            | 0.1058                 |
| c8i.xlarge    | 4    | 8            | 0.1874                 |
| m8i.xlarge    | 4    | 16           | 0.2117                 |
| r8i.xlarge    | 4    | 32           | 0.2778                 |
| m8i.2xlarge   | 8    | 32           | 0.4234                 |

Cross-family differences at xlarge: m8i to c8i saves $0.024/hr; r8i to m8i saves $0.066/hr. A threshold of 0.01 means a 5-pod node requires $0.05/hr savings to consolidate. With the default:

- m8i-to-c8i moves are blocked ($0.024 < $0.05 required)
- r8i-to-m8i moves are allowed ($0.066 >= $0.05 required)

Whether this is desirable depends on the workload. Users who want to allow m8i-to-c8i moves can lower the threshold. Users who want to block r8i-to-m8i moves can raise the threshold. The default is conservative: it prevents more churn than it allows.

## Appendix B: Tuning Guide

The threshold is dollars per hour per unit of disruption cost. For nodes with default-priority pods:

| Threshold | 5-pod node requires | 10-pod node requires | 50-pod node requires |
|-----------|---------------------|----------------------|----------------------|
| 0.005     | $0.025/hr           | $0.05/hr             | $0.25/hr             |
| 0.01      | $0.05/hr            | $0.10/hr             | $0.50/hr             |
| 0.02      | $0.10/hr            | $0.20/hr             | $1.00/hr             |

Start with the default.

**When to increase:** Nodes consolidate multiple times per day for small savings (less than $0.10/hr each). Pods are disrupted more frequently than workload owners expect.

**When to decrease:** Obvious cost savings opportunities are not taken. Nodes run for hours at low utilization without consolidating.

Most clusters should not need to change the default. The threshold is designed to block the clearly-wrong moves (like issue #7146) while allowing reasonable consolidation to proceed.
