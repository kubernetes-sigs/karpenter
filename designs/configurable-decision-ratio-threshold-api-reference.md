# DecisionRatioThreshold API Reference

## Overview

The `DecisionRatioThreshold` field in the NodePool API allows operators to configure a custom threshold for consolidation decisions when using the `WhenCostJustifiesDisruption` policy. This field provides fine-grained control over the cost-benefit tradeoff for consolidation operations.

## API Field

### Location

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  disruption:
    decisionRatioThreshold: <float>
```

### Field Specification

| Field | Type | Required | Default | Validation |
|-------|------|----------|---------|------------|
| `decisionRatioThreshold` | `float64` | No | `1.0` | Must be greater than 0 |

### Description

`DecisionRatioThreshold` is the minimum decision ratio required for consolidation when `ConsolidateWhen` is set to `WhenCostJustifiesDisruption`.

The decision ratio compares normalized cost savings to normalized disruption cost. A threshold of 1.0 represents the break-even point where cost savings equal disruption cost.

- **Values greater than 1.0** make consolidation more conservative (require higher cost savings)
- **Values less than 1.0** make consolidation more aggressive (allow lower cost savings)

This field only applies when `ConsolidateWhen` is `WhenCostJustifiesDisruption`. When `ConsolidateWhen` is `WhenEmpty` or `WhenEmptyOrUnderutilized`, this field is ignored.

## Usage Examples

### Conservative Consolidation (Latency-Sensitive Workloads)

For workloads where disruption has high impact (e.g., latency-sensitive services, real-time applications), use a higher threshold to ensure consolidation only occurs when cost savings significantly outweigh disruption costs.

**Example: Threshold 1.5**

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: latency-sensitive
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
    decisionRatioThreshold: 1.5
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
```

**Effect**: Consolidation only occurs when cost savings are at least 1.5 times the disruption cost. This reduces the frequency of consolidation operations, minimizing disruption to running workloads.

**Example: Threshold 2.0**

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: production-critical
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
    decisionRatioThreshold: 2.0
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
```

**Effect**: Consolidation only occurs when cost savings are at least 2.0 times the disruption cost. This is highly conservative and suitable for mission-critical workloads where stability is paramount.

### Aggressive Consolidation (Cost-Sensitive Workloads)

For workloads where cost optimization is the primary concern (e.g., batch processing, development environments, non-critical services), use a lower threshold to allow consolidation even when cost savings are less than disruption costs.

**Example: Threshold 0.5**

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: batch-workloads
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
    decisionRatioThreshold: 0.5
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot"]
```

**Effect**: Consolidation occurs when cost savings are at least half the disruption cost. This allows more frequent consolidation, prioritizing cost savings over stability.

**Example: Threshold 0.7**

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: dev-environment
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
    decisionRatioThreshold: 0.7
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
```

**Effect**: Consolidation occurs when cost savings are at least 70% of the disruption cost. This is moderately aggressive and suitable for development or staging environments.

### Default Behavior (Backward Compatible)

When `decisionRatioThreshold` is omitted, the system defaults to 1.0, which represents the break-even point.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default-pool
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
    # decisionRatioThreshold omitted, defaults to 1.0
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
```

**Effect**: Consolidation occurs when cost savings equal or exceed disruption costs. This maintains backward compatibility with existing configurations.

### Multi-NodePool Configuration

Different NodePools can have different thresholds, allowing you to apply appropriate cost-disruption tradeoffs to different workload types within the same cluster.

```yaml
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: production
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
    decisionRatioThreshold: 2.0  # Conservative
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: production
      requirements:
        - key: environment
          operator: In
          values: ["production"]
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: batch
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
    decisionRatioThreshold: 0.5  # Aggressive
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: batch
      requirements:
        - key: workload-type
          operator: In
          values: ["batch"]
```

## Validation Rules

The NodePool API enforces the following validation rules for `decisionRatioThreshold`:

1. **Must be positive**: Values must be greater than 0 (zero and negative values are rejected)
2. **Type**: Must be a floating-point number
3. **Optional**: Field can be omitted (defaults to 1.0)

### Valid Values

- ✅ `0.001` - Minimum practical value
- ✅ `0.5` - Aggressive consolidation
- ✅ `0.7` - Moderately aggressive
- ✅ `1.0` - Break-even (default)
- ✅ `1.5` - Conservative consolidation
- ✅ `2.0` - Highly conservative
- ✅ `100.0` - Extremely conservative (blocks nearly all consolidation)

### Invalid Values

- ❌ `0` - Zero is not allowed (validation error)
- ❌ `-0.5` - Negative values are not allowed (validation error)
- ❌ `-1.0` - Negative values are not allowed (validation error)

### Error Messages

When an invalid value is provided, the API returns a validation error:

```
The NodePool "example" is invalid: spec.disruption.decisionRatioThreshold: Invalid value: 0: spec.disruption.decisionRatioThreshold in body should be greater than 0
```

## Behavior Details

### When the Threshold Applies

The `decisionRatioThreshold` is **only used** when:
- `spec.disruption.consolidateWhen` is set to `WhenCostJustifiesDisruption`

The threshold is **ignored** when:
- `spec.disruption.consolidateWhen` is set to `WhenEmpty`
- `spec.disruption.consolidateWhen` is set to `WhenEmptyOrUnderutilized`

### Decision Ratio Calculation

The decision ratio is calculated as:

```
Decision Ratio = (Normalized Cost Savings) / (Normalized Disruption Cost)
```

Where:
- **Normalized Cost Savings**: The hourly cost savings from consolidation, expressed as a fraction of total NodePool cost
- **Normalized Disruption Cost**: The total disruption cost (based on pod deletion costs and priorities), expressed as a fraction of total NodePool disruption

### Consolidation Decision Logic

For each consolidation candidate:

1. Calculate the decision ratio
2. Compare the decision ratio against the configured threshold
3. Execute consolidation if: `Decision Ratio >= DecisionRatioThreshold`
4. Skip consolidation if: `Decision Ratio < DecisionRatioThreshold`

### Delete Ratio Optimization

The threshold is also used for an optimization called "delete ratio filtering":

1. Before generating expensive consolidation moves, compute an upper-bound "delete ratio" for each candidate
2. If the delete ratio is less than the threshold, skip move generation for that candidate
3. This optimization reduces computational overhead by avoiding move generation for candidates that would be filtered out anyway

## Observability

### Metrics

Karpenter emits the following metrics related to the decision ratio threshold:

#### `karpenter_consolidation_decision_ratio`

Histogram of decision ratios for evaluated consolidation moves.

**Labels:**
- `nodepool`: NodePool name
- `policy`: ConsolidateWhen policy value
- `threshold`: Configured DecisionRatioThreshold value (formatted as "%.2f")
- `move_type`: Type of consolidation move (delete, replace)

#### `karpenter_consolidation_moves_above_threshold_total`

Counter of consolidation moves with decision ratio >= threshold.

**Labels:**
- `nodepool`: NodePool name
- `policy`: ConsolidateWhen policy value
- `threshold`: Configured DecisionRatioThreshold value (formatted as "%.2f")

#### `karpenter_consolidation_moves_below_threshold_total`

Counter of consolidation moves with decision ratio < threshold (skipped moves).

**Labels:**
- `nodepool`: NodePool name
- `policy`: ConsolidateWhen policy value
- `threshold`: Configured DecisionRatioThreshold value (formatted as "%.2f")

#### `karpenter_consolidation_moves_skipped_by_delete_ratio_total`

Counter of consolidation moves skipped due to low delete ratio (optimization).

**Labels:**
- `nodepool`: NodePool name
- `policy`: ConsolidateWhen policy value
- `threshold`: Configured DecisionRatioThreshold value (formatted as "%.2f")

### Example Prometheus Queries

**View decision ratio distribution for a specific threshold:**
```promql
histogram_quantile(0.95, 
  rate(karpenter_consolidation_decision_ratio_bucket{threshold="1.50"}[5m])
)
```

**Compare consolidation rates across different thresholds:**
```promql
rate(karpenter_consolidation_moves_above_threshold_total[5m])
```

**Calculate the percentage of moves skipped by threshold:**
```promql
rate(karpenter_consolidation_moves_below_threshold_total[5m]) 
/ 
(rate(karpenter_consolidation_moves_above_threshold_total[5m]) + 
 rate(karpenter_consolidation_moves_below_threshold_total[5m]))
```

## Best Practices

### Choosing a Threshold

Consider the following factors when choosing a threshold:

1. **Workload Sensitivity**: How sensitive are your workloads to disruption?
   - High sensitivity → Higher threshold (1.5-2.0)
   - Low sensitivity → Lower threshold (0.5-0.7)

2. **Cost Optimization Goals**: How important is cost reduction?
   - Cost is critical → Lower threshold (0.5-0.7)
   - Stability is critical → Higher threshold (1.5-2.0)

3. **Consolidation Frequency**: How often do you want consolidation to occur?
   - Frequent consolidation → Lower threshold
   - Infrequent consolidation → Higher threshold

4. **Pod Disruption Budgets**: Do you have strict PDBs?
   - Strict PDBs → Higher threshold (to reduce disruption attempts)
   - Relaxed PDBs → Lower threshold

### Testing and Tuning

1. **Start with the default** (1.0) and observe consolidation behavior
2. **Monitor metrics** to understand decision ratio distribution
3. **Adjust gradually** (e.g., 1.0 → 1.2 → 1.5) and observe impact
4. **Use different thresholds** for different NodePools based on workload characteristics

### Common Patterns

**Pattern 1: Environment-Based Thresholds**
- Production: 2.0 (highly conservative)
- Staging: 1.0 (default)
- Development: 0.5 (aggressive)

**Pattern 2: Workload-Type-Based Thresholds**
- Stateful services: 2.0 (minimize disruption)
- Stateless services: 1.0 (balanced)
- Batch jobs: 0.5 (optimize cost)

**Pattern 3: Time-Based Thresholds**
- Use higher thresholds during business hours
- Use lower thresholds during off-hours
- (Note: This requires updating the NodePool configuration, which can be automated)

## Migration Guide

### Upgrading from Existing Configurations

If you have existing NodePools using `WhenCostJustifiesDisruption`, no changes are required. The system will automatically default to a threshold of 1.0, maintaining existing behavior.

**Before (v1beta1 or v1 without threshold):**
```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
```

**After (v1 with explicit threshold):**
```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidateWhen: WhenCostJustifiesDisruption
    decisionRatioThreshold: 1.0  # Explicit, but same behavior
```

### Adopting Custom Thresholds

To adopt custom thresholds:

1. **Identify workload characteristics** for each NodePool
2. **Choose appropriate thresholds** based on best practices
3. **Update NodePool configurations** with the new threshold values
4. **Monitor metrics** to validate the impact
5. **Adjust as needed** based on observed behavior

## Related Documentation

- [Consolidation Decision Ratio Control Design](./consolidation-decision-ratio-control.md)
- [Disruption Controls](./disruption-controls.md)
- [NodePool API v1](./v1-api.md)

## Support

For questions or issues related to the `decisionRatioThreshold` field:
- Slack: [#karpenter](https://kubernetes.slack.com/archives/C02SFFZSA2K) in Kubernetes Slack
- GitHub Issues: [kubernetes-sigs/karpenter](https://github.com/kubernetes-sigs/karpenter/issues)
