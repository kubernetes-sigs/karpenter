# Consolidation Savings Threshold

## Definitions and Current Algorithm

Karpenter computes a **disruption cost** for each node to order consolidation candidates from easiest to hardest to disrupt. The complete formula (see `pkg/utils/disruption/disruption.go`):

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

**Issue #7146 with proposed algorithm (threshold = 1.0, meaning $0.01/hr per disruption cost unit):**

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 5 * 1.0 = 5.0
Replacement: m7i-flex.large @ $0.080/hr
Savings: $0.006/hr
Required: $0.01 * 5.0 = $0.05/hr

Result: NO CONSOLIDATE ($0.006 < $0.05)
```

The cascade is blocked at generation 1. Pods are not disrupted for less than a penny.

**Decision boundary: when does consolidation proceed?**

The decision flips when savings reaches $0.05/hr:

```
Node: m6a.large @ $0.086/hr, ~5 pods
Disruption cost: 5.0
Replacement: (hypothetical) @ $0.036/hr
Savings: $0.05/hr
Required: $0.05/hr

Result: CONSOLIDATE ($0.05 >= $0.05)
```

The threshold creates a clear decision boundary: for this 5-pod node with threshold 1.0, consolidation requires at least $0.05/hr savings.

**Multi-node consolidation:**

When consolidating multiple nodes to one replacement, sum the disruption costs:

```
Node A: $0.50/hr, 3 pods -> disruption cost = 3.0
Node B: $0.50/hr, 7 pods -> disruption cost = 7.0
Total disruption cost: 10.0
Required savings: $0.01 * 10.0 = $0.10/hr

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

### Recommendation

Option 1 (Disruption-Normalized Threshold) addresses the core problem: savings should justify disruption. Options 2-4 ignore pod-level disruption cost entirely.

Options 2-4 could layer on top of Option 1 for defense in depth. In particular, a minimum absolute savings floor (Option 3) might prevent edge cases where very small nodes with few pods still experience marginal-savings churn. We defer this layering until we learn from feedback on Option 1.

**Performance note:** The threshold check is asymptotically O(P) for a cluster with P total pods. We compute each node's disruption cost once by summing its pod costs, then apply O(1) threshold comparisons to each node. Because this is just arithmetic on already-computed values, multiple filters (percentage, absolute, disruption-normalized) could be evaluated in parallel and AND'd together with negligible overhead. A node rejected by any filter can be rejected by all, so there's even a future option of applying the most informative filters first. Layering them is computationally cheap.

## Configuration

We believe a default threshold (discussed in Cloud Provider Variance) will work for most users without configuration. This value blocks consolidations with marginal savings, eliminating the worst cascading behavior while preserving high-value consolidation opportunities.

We defer exposing a user-configurable parameter until we have feedback. If users report that the default is too aggressive (blocking legitimate consolidations) or too permissive (still allowing excessive churn), we can add a NodePool-level field in a subsequent release.

## Cloud Provider Variance

This feature is vendor-neutral. The threshold mechanism applies identically across cloud providers: compute disruption cost from pod metadata, compare savings against threshold, proceed or reject.

Our analysis on EC2 is based on the observation that if we take all of the instances in a NodePool and sort by price, there's a minimum unit of price difference that we can distinguish by choosing different instances. On EC2, this minimum difference is approximately $0.01/hr, leading us to recommend a default threshold of 1.0 (meaning $0.01/hr per unit of disruption cost).

To the extent that other environments like GCP, Azure, and others have similar minimum price differences, we expect the same threshold to work about as well. We welcome feedback from users on other cloud providers during implementation.

## Interaction with Spot Consolidation

The savings threshold filter applies before the 15-candidate flexibility check from [spot-consolidation.md](spot-consolidation.md). Candidates that don't meet the savings threshold are excluded from the candidate pool before we count whether 15+ options remain.

This means consolidation ignores marginal savings options when building the spot candidate set. This is intentional: if a candidate doesn't meet the savings bar, it shouldn't count toward the flexibility requirement either.

See Appendix A for a worked example.

## Interaction with Other Disruption Controls

The threshold composes with NodePool budgets naturally. Budgets limit disruption concurrency and timing; the threshold gates whether individual consolidations are worth doing.

Existing filters run before the threshold check:
- **consolidateAfter**: Nodes must have the `Consolidatable` condition
- **ConsolidationPolicy**: Must be `WhenEmptyOrUnderutilized`
- **PodDisruptionBudgets**: Respected during scheduling simulation

These filters determine which nodes are candidates. The threshold determines whether a proposed consolidation yields sufficient savings.

## DaemonSet Pod Handling

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

## Observability

**Logging:** Log blocked consolidations at info level:
```
consolidation blocked: savings $0.006/hr below required $0.05/hr (disruption_cost=5.0, threshold=1.0)
```

**Proposed metric:**
```
karpenter_consolidation_threshold_blocked_total{nodepool="default"}
```

Counter increments each time consolidation is rejected due to threshold. Operators can alert on this to detect overly aggressive thresholds.

## Implementation Scope

The threshold check integrates into the existing consolidation path:

**Modified packages:**
- `pkg/controllers/disruption/consolidation.go`: Add threshold check in `computeConsolidation()`

**Key changes:**
- In `computeConsolidation()`, after computing candidate prices, calculate `required_savings = threshold * disruption_cost` for all source nodes
- Filter replacement options to those where `sum(candidatePrices) - replacementPrice >= required_savings`
- Add metric increment when consolidation is blocked

Estimated scope: ~100-150 lines including tests. No architectural changes; the threshold is an additional filter in the existing flow.

## Future Work

Several extensions are deferred to keep this RFC focused:

- **Node base cost**: The current disruption cost formula sums per-pod costs, meaning empty nodes have disruption cost of zero. A base cost (e.g., 1.0) would capture operational overhead independent of pods: connection draining, API server load, cache invalidation. We defer this until we have cases where empty-node consolidation behavior is problematic.
- **User-configurable threshold**: Expose the threshold as a NodePool-level field if the default of 1.0 proves unsuitable for common workloads.
- **Destination lifetime consideration**: Factor in replacement node's remaining lifetime when evaluating savings.
- **Adaptive threshold tuning**: AIMD-style adjustment based on consolidation outcomes (too much churn = increase threshold; costs stagnant = decrease).
- **Spot candidate count interaction**: Analyze whether the 15-candidate requirement should be relaxed when savings threshold is high.
- **Learning from outcomes**: Use historical consolidation data to calibrate thresholds automatically.

## Appendix A: Spot Consolidation Example

For spot-to-spot single-node consolidation:

1. Find instance types cheaper than current node
2. Filter to types where `savings >= required_savings` for this source node
3. Require 15+ remaining candidates (per spot-consolidation.md)
4. Send viable candidates to PCO for selection

**Example:**

```
Source: m5.xlarge spot @ $0.07/hr, 8 pods, threshold 1.0
  Disruption cost: 8 * 1.0 = 8.0
  Required savings: $0.01 * 8.0 = $0.08/hr

Candidate pool (50 instance types cheaper than $0.07/hr):
  - 12 types save $0.02-$0.05/hr  -> filtered out (below $0.08 threshold)
  - 18 types save $0.08-$0.15/hr  -> pass threshold
  - 20 types save $0.15-$0.30/hr  -> pass threshold

Candidates passing threshold: 38
Required minimum: 15

Result: PROCEED with 38 candidates sent to PCO
```

The 15-candidate requirement applies to candidates that pass the savings threshold. If fewer than 15 candidates yield sufficient savings, no spot-to-spot consolidation occurs.
