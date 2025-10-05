# Consolidation Price Improvement Factor

## Problem Statement

Karpenter's consolidation logic creates excessive node churn by replacing nodes for marginal cost savings, leading to unnecessary pod cycling where workloads experience multiple pod rescheduling events per hour as nodes are repeatedly consolidated for savings as small as 5-15%.

Users have reported nodes being disrupted and replaced every 5-10 minutes, causing:
- Unnecessary pod rescheduling and application disruption
- Reduced system reliability and availability
- User frustration requiring workarounds (disabling spot-to-spot consolidation, extensive PDB usage, extended consolidateAfter periods)

**Reference Issue**: https://github.com/aws/karpenter-provider-aws/issues/7146

The core problem is that Karpenter consolidates nodes whenever a cheaper alternative exists, without requiring **meaningful cost improvement**. A node running at $0.95/hr will be immediately replaced by one at $0.90/hr (5% savings), then again by one at $0.85/hr (5.5% savings), creating a cascade of disruptions for minimal benefit.

## Proposed Solution

### Overview

Introduce a **Price Improvement Factor** configuration option that allows users to require replacement nodes to offer **meaningful cost savings** before consolidation occurs. This factor acts as a threshold: only when `replacement_price < (current_price × factor)` will consolidation proceed.

For example, a factor of **0.8** would require at least **20% cost savings**, while the default of **1.0** maintains current behavior (consolidate for any savings).

### Configuration Levels

The price improvement factor will be configurable at **two levels** with the following precedence:

1. **NodePool-level** (highest priority) - Per-workload control
2. **Operator-level** (fallback) - Cluster-wide default

This dual-level approach provides:
- **Simplicity for common case**: Set operator flag once, applies cluster-wide
- **Flexibility for advanced case**: Override per-NodePool for workload-specific needs

#### NodePool-Level Configuration

Add field to the `Disruption` spec (follows existing pattern of `consolidateAfter`, `consolidationPolicy`):

```go
type Disruption struct {
    ConsolidateAfter NillableDuration `json:"consolidateAfter"`
    ConsolidationPolicy ConsolidationPolicy `json:"consolidationPolicy,omitempty"`
    Budgets []Budget `json:"budgets,omitempty" hash:"ignore"`

    // ConsolidationPriceImprovementFactor is the minimum cost savings required
    // for consolidation to occur. Only consolidate when replacement nodes cost
    // less than (current_price × factor).
    //
    // If not specified, falls back to the operator-level setting.
    //
    // Examples:
    //   1.0 = Consolidate for any cost savings (legacy behavior)
    //   0.8 = Require 20% cost savings
    //   0.5 = Require 50% cost savings (very conservative)
    //   0.0 = Disable price-based consolidation
    //
    // +kubebuilder:validation:Minimum:=0.0
    // +kubebuilder:validation:Maximum:=1.0
    // +optional
    ConsolidationPriceImprovementFactor *float64 `json:"consolidationPriceImprovementFactor,omitempty"`
}
```

#### Operator-Level Configuration

Provides cluster-wide default when NodePool doesn't specify:

```go
type Options struct {
    // ConsolidationPriceImprovementFactor determines the default minimum cost
    // savings required for consolidation. This value is used when a NodePool
    // doesn't specify consolidationPriceImprovementFactor.
    //
    // Range: [0.0, 1.0]
    // Default: 1.0 (backward compatible)
    ConsolidationPriceImprovementFactor float64
}
```

**CLI Flag:**
```bash
--consolidation-price-improvement-factor=0.8
```

**Environment Variable:**
```bash
CONSOLIDATION_PRICE_IMPROVEMENT_FACTOR=0.8
```

### Implementation Approach

The price improvement factor filters instance type options during consolidation decisions:

1. **Resolve configuration value** (precedence order):
   - If `NodePool.spec.disruption.consolidationPriceImprovementFactor` is set, use it
   - Else, use operator-level `--consolidation-price-improvement-factor`
   - Else, use default `1.0` which maintains current behavior.

2. **Calculate threshold**: `maxAllowedPrice = currentPrice × priceImprovementFactor`

3. **Filter instances**: Only consider instances where `launchPrice < maxAllowedPrice`

4. **Apply existing validation**: Ensure min values requirements are still satisfied

**Draft Implementation**: https://github.com/kubernetes-sigs/karpenter/pull/2561

## Behavior Examples

### Current Behavior (Factor = 1.0, default)

```
Scenario: Node @ $1.00/hour, replacement @ $0.95/hour (5% savings)
Threshold: $1.00 × 1.0 = $1.00
Decision: CONSOLIDATE ($0.95 < $1.00)
Result:  Pod restarted, then 5 minutes later another consolidation to $0.90/hour
Impact:  Excessive churn, multiple pod restarts
```

### With Price Improvement Factor (Factor = 0.8)

```
Scenario: Node @ $1.00/hour, replacement @ $0.95/hour (5% savings)
Threshold: $1.00 × 0.8 = $0.80
Decision: NO CONSOLIDATE ($0.95 > $0.80)
Result:  Node remains stable, no pod restart
Impact:  System stability maintained

Scenario: Node @ $1.00/hour, replacement @ $0.70/hour (30% savings)
Threshold: $1.00 × 0.8 = $0.80
Decision: CONSOLIDATE ($0.70 < $0.80)
Result:  Meaningful cost savings achieved
Impact:  Cost optimization with minimal churn
```

## Configuration Examples

### Example 1: Cluster-Wide Default (Simple Case)

Set operator-level default to reduce churn across all NodePools:

```yaml
# Karpenter deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: karpenter
spec:
  template:
    spec:
      containers:
      - name: controller
        env:
        - name: CONSOLIDATION_PRICE_IMPROVEMENT_FACTOR
          value: "0.8"  # Require 20% cost savings cluster-wide
```

All NodePools inherit this setting automatically.

### Example 2: Workload-Specific Overrides (Advanced Case)

Different consolidation aggressiveness per workload:

```yaml
# Operator default: moderate (10% savings required)
env:
  - name: CONSOLIDATION_PRICE_IMPROVEMENT_FACTOR
    value: "0.9"
---
# Stateful workload: very conservative
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: database-pool
spec:
  disruption:
    consolidationPriceImprovementFactor: 0.5  # Require 50% savings
  template:
    spec:
      nodeSelector:
        workload-type: database
---
# Web workload: uses operator default (0.9)
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: web-pool
spec:
  template:
    spec:
      nodeSelector:
        workload-type: web
---
# Batch workload: aggressive cost optimization
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: batch-pool
spec:
  disruption:
    consolidationPriceImprovementFactor: 1.0  # Any cost savings
  template:
    spec:
      nodeSelector:
        workload-type: batch
```

### Example 3: Gradual Adoption

Start with no change, gradually roll out:

```yaml
# Phase 1: No change (operator default 1.0)
env:
  - name: CONSOLIDATION_PRICE_IMPROVEMENT_FACTOR
    value: "1.0"

# Phase 2: Test on dev workloads
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: dev-pool
spec:
  disruption:
    consolidationPriceImprovementFactor: 0.8  # Test with 20% threshold

# Phase 3: Roll out cluster-wide
# (change operator default to 0.8, remove NodePool overrides)
```

## Backward Compatibility

- **No breaking changes**: Default value of `1.0` maintains existing behavior
- **Opt-in adoption**: Users experiencing churn can lower the factor via operator flag or NodePool spec
- **CRD change**: Adds optional field to NodePool Disruption spec (backward compatible)

## Alternatives Considered

### 1. Extended ConsolidateAfter period

**Approach**: Prevent consolidation of recently launched nodes for a fixed duration (e.g., 4 hours).

**Pros**:
- Simple to understand and implement
- Directly addresses rapid consolidation

**Cons**:
- 👎👎 Still eventually converges to lowest price (just slower)
- 👎 Consolidatable nodes sit idle unnecessarily long
- 👎 Doesn't prevent marginal consolidation, just delays it

**Why not chosen**: Delays the problem rather than solving it.

### 2. Absolute Cost Threshold

**Approach**: Only consolidate if savings exceed a fixed dollar amount (e.g., $0.10/hour).

**Pros**:
- Easy to reason about in dollar terms

**Cons**:
- 👎 Threshold that works for small instances ($0.50/hour) doesn't work for large ones ($5.00/hour)
- 👎 Requires different values for different instance sizes
- 👎 Not portable across cloud providers with different pricing

**Why not chosen**: Price improvement factor scales naturally with instance price.

### 3. Operator-Level Only

**Approach**: Support only operator-level configuration, no NodePool-level override.

**Pros**:
- Simpler implementation (no CRD changes)
- Cluster-wide consistency
- Easier to reason about

**Cons**:
- 👎 No workload-specific control
- 👎 Forces same behavior on all NodePools (stateful and stateless)
- 👎 Can't gradually test on subset of nodes
- 👎 Doesn't follow existing Karpenter patterns (consolidateAfter, consolidationPolicy are NodePool-level)

**Why not chosen**: Lacks flexibility for diverse workload requirements. Users with both latency-sensitive and batch workloads need different thresholds.

### 4. NodePool-Level Only

**Approach**: Support only NodePool-level configuration, no operator-level default.

**Pros**:
- Follows existing pattern (consolidateAfter, consolidationPolicy)
- Workload-specific control

**Cons**:
- 👎 Requires setting on every NodePool (config duplication)
- 👎 No cluster-wide default for common case
- 👎 Users forget to set it on new NodePools

**Why not chosen**: Forces users to duplicate configuration across NodePools. The dual-level approach (recommended) provides better UX with operator-level default + NodePool override.

### 5. Dynamic Adjustment Based on Churn Rate

**Approach**: Automatically adjust factor based on observed consolidation frequency.

**Pros**:
- Self-tuning system

**Cons**:
- 👎👎 Complex algorithm with unpredictable behavior
- 👎 Difficult to debug and reason about
- 👎 May oscillate or have unintended feedback loops

**Why not chosen**: Explicit configuration is more predictable and debuggable.

## Relationship to Existing Designs

### Connection to Spot Consolidation Design

This implementation relates to Option 2 ("Price Improvement Factor") discussed in [spot-consolidation.md](spot-consolidation.md). As noted in that document:

> "Regardless of the decision made to solve the spot consolidation problem, we'd likely want to implement a price improvement in the future to prevent consolidation from interrupting nodes to make marginal improvements."

This design implements that recommendation as a **core consolidation feature** applicable to both spot and on-demand instances, not just spot-to-spot consolidation.

### Integration with Disruption Controls

The price improvement factor complements [disruption-controls.md](disruption-controls.md) by providing another dimension of consolidation control:

- **Disruption budgets**: Control *how many* nodes can be disrupted at once
- **consolidateAfter**: Control *when* consolidation can start
- **Price improvement factor**: Control *which* consolidations are worthwhile

These controls are orthogonal and work together to give users comprehensive control over disruption behavior.


## Recommendation

Implement the **Price Improvement Factor** with **dual-level configuration** (NodePool + operator) and default value **1.0**:

* 👍👍👍 Directly addresses the excessive consolidation churn problem reported in #7146
* 👍👍 Simple to understand and explain: "only consolidate when savings exceed X%"
* 👍👍 Best of both worlds: cluster-wide default + workload-specific overrides
* 👍 Follows Karpenter patterns: disruption settings in NodePool spec (like consolidateAfter)
* 👍 Avoids config duplication: operator-level default inherited by all NodePools
* 👍 Enables gradual adoption: test on subset of NodePools before cluster-wide rollout
* 👍 Cloud-agnostic solution applicable to all providers
* 👍 Fully backward compatible with no breaking changes
* 👍 Works seamlessly with existing spot-to-spot minimum flexibility requirements
* 👍 Reduces operational burden (fewer PDBs, do-not-consolidate annotations, extended consolidateAfter periods)

This dual-level approach provides maximum flexibility while maintaining simplicity for the common case (set once at operator level), giving users comprehensive control over consolidation behavior.
