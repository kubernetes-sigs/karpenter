# Consolidation Savings Threshold

## Problem

Karpenter consolidation removes nodes when doing so saves money. But consolidating a non-empty node has a cost: pods get disrupted. The current algorithm consolidates whenever `replacement_price < current_price`, ignoring disruption cost entirely. This causes excessive churn when savings are marginal.

**Case study:** [aws/karpenter-provider-aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146)

A cluster experienced cascading consolidation during off-peak hours. Nodes were disrupted, replacement nodes re-disrupted within minutes, some pods restarted 4 times. The algorithm saw:

```
Node: m6a.large @ $0.086/hr, 5 pods
Replacement: m7i-flex.large @ $0.080/hr
Savings: $0.006/hr -> CONSOLIDATE
```

Six-tenths of a cent per hour. For 5 pods. The community has proposed workarounds (disable spot-to-spot, add PDBs, multi-hour consolidateAfter delays), but none address the root cause: Karpenter consolidates for any positive savings.

**Insight:** When we consolidate, we're trading disruption for savings. Karpenter already computes a disruption cost for each node (see `pkg/utils/disruption/disruption.go`). Today this cost only orders candidates. This RFC proposes also using it as a gate: require savings to exceed a threshold proportional to disruption cost.

## Design Options

### 1. Disruption-Scaled Threshold [Recommended]

Require `savings >= threshold * disruption_cost` to consolidate.

With threshold 0.01 and 5 pods (disruption cost ~5.0), require $0.05/hr savings. This filters marginal candidates while allowing meaningful consolidation.

**Issue #7146 with this approach:**

```
Node: m6a.large @ $0.086/hr, 5 pods
Disruption cost: 5.0
Required savings: 0.01 * 5.0 = $0.05/hr
Actual savings: $0.006/hr

Result: NO CONSOLIDATE ($0.006 < $0.05)
```

The cascade is blocked. The 5-pod node requires at least $0.05/hr savings. If savings were $0.07/hr, consolidation proceeds.

**Multi-node consolidation:** Sum disruption costs. Consolidating a 3-pod node and 7-pod node together requires savings proportional to 10 pods of disruption.

**Empty nodes:** Disruption cost is 0, so required savings is 0. Empty nodes always consolidate.

* Filters marginal moves while allowing valuable consolidation
* Scales naturally: more pods means more savings required
* Uses existing disruption cost calculation
* Introduces a parameter users must understand

### 2. Flat Percentage Threshold

Require replacement to be X% cheaper (e.g., 20%). Simple to explain, but percentages interact poorly with spot's low absolute prices. A $0.07/hr spot node saving 20% is $0.014/hr - still marginal for 8 pods.

* Easy to explain
* Ignores pod count: 5 pods and 50 pods have identical thresholds
* Hard to pick a value that works across node sizes

### 3. Minimum Absolute Savings

Require a fixed dollar amount (e.g., $0.10/hr). Simple and predictable, but a single value cannot suit both small and large nodes.

* Dead simple
* Ignores pod count
* Blocks legitimate consolidation of small nodes

### 4. Time-Based Dampener

Prevent consolidation of recently-launched nodes (e.g., 4 hours). Delays the problem rather than solving it. Once the time passes, all marginal candidates are back in play.

* Simple
* Blocks good consolidations alongside bad ones
* Doesn't distinguish high-value from low-value moves

## Recommendation

Option 1. Savings should justify disruption. The other options either ignore pod count or fail to distinguish valuable moves from marginal ones.

### API

NodePool-level field:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  disruption:
    consolidationSavingsThreshold: "0.01"  # default
```

The threshold is dollars per hour per unit of disruption cost. For typical workloads with default-priority pods, this approximates "dollars per hour per pod." A 5-pod node requires $0.05/hr; a 20-pod node requires $0.20/hr.

The default value (0.01) is a starting point. We expect to tune this based on production feedback. Users who want legacy behavior can set it to 0.

### Spot Interaction

This filter runs before the spot 15-candidate flexibility check (see spot-consolidation.md). If filtering leaves fewer than 15 candidates, no spot-to-spot consolidation occurs.

### Lifecycle

Alpha initially. Graduate after user feedback confirms the default threshold works across diverse workloads.

## Future Work

The savings and disruption cost estimates can be improved over time:

- **Break-even time model:** Require savings to pay back disruption within N hours rather than rate-based threshold
- **Pod-lifetime awareness:** Weight long-running pods higher in disruption cost
- **Destination node lifetime:** Consider that pods moving to a node near expiration will be disrupted again soon

These are orthogonal improvements to the estimates, not changes to this design.
