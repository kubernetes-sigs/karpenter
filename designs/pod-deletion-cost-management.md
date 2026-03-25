# Pod Deletion Cost Management

## Overview

Pod Deletion Cost Management is a feature that enables Karpenter to automatically manage the `controller.kubernetes.io/pod-deletion-cost` annotation on pods running on Karpenter-managed nodes. This annotation influences Kubernetes' pod selection during scale-in events, allowing Karpenter to optimize which pods are terminated first based on configurable ranking strategies.

## Motivation

During Kubernetes scale-in operations (such as ReplicaSet downscaling or node draining), the Kubernetes controller manager uses the `controller.kubernetes.io/pod-deletion-cost` annotation to determine which pods should be deleted first. Pods with lower deletion cost values are prioritized for termination. By automatically managing this annotation, Karpenter can:

- **Optimize cost**: Prioritize termination of pods on more expensive nodes
- **Improve efficiency**: Target pods on underutilized nodes first
- **Protect critical workloads**: Ensure nodes with do-not-disrupt pods are deprioritized
- **Provide flexibility**: Support multiple ranking strategies for different use cases

## Configuration

### Feature Enablement

The Pod Deletion Cost Management feature is controlled via environment variables:

```yaml
env:
  - name: POD_DELETION_COST_ENABLED
    value: "true"
  - name: POD_DELETION_COST_RANKING_STRATEGY
    value: "UnallocatedVCPUPerPodCost"
  - name: POD_DELETION_COST_CHANGE_DETECTION
    value: "true"
```

### Configuration Options

| Environment Variable | Description | Default | Valid Values |
|---------------------|-------------|---------|--------------|
| `POD_DELETION_COST_ENABLED` | Enable/disable the feature | `false` | `true`, `false` |
| `POD_DELETION_COST_RANKING_STRATEGY` | Algorithm for ranking nodes | `Random` | `Random`, `LargestToSmallest`, `SmallestToLargest`, `UnallocatedVCPUPerPodCost` |
| `POD_DELETION_COST_CHANGE_DETECTION` | Skip ranking when cluster state unchanged | `true` | `true`, `false` |

## Ranking Strategies

### Random

Assigns node ranks in random order, providing no specific optimization but ensuring varied pod deletion patterns.

**Use Case**: Testing, development environments, or when no specific optimization is needed.

**Example**:
```
Nodes: node-a, node-b, node-c
Ranks: node-c=-1000, node-a=-999, node-b=-998 (random order)
```

### LargestToSmallest

Ranks nodes by total allocatable capacity (CPU + Memory), with larger nodes receiving lower ranks (higher deletion priority).

**Use Case**: Prefer terminating pods on larger, potentially more expensive instances first to reduce costs.

**Calculation**:
- Normalized capacity = (CPU cores) + (Memory GB / 4)
- Nodes sorted by capacity descending
- Lower ranks assigned to larger nodes

**Example**:
```
node-large:  32 CPU, 128GB RAM → capacity = 32 + 32 = 64 → rank = -1000
node-medium: 16 CPU, 64GB RAM  → capacity = 16 + 16 = 32 → rank = -999
node-small:  8 CPU, 32GB RAM   → capacity = 8 + 8 = 16   → rank = -998
```

### SmallestToLargest

Ranks nodes by total allocatable capacity, with smaller nodes receiving lower ranks (higher deletion priority).

**Use Case**: Prefer terminating pods on smaller instances first, consolidating workloads onto larger nodes.

**Calculation**:
- Same capacity calculation as LargestToSmallest
- Nodes sorted by capacity ascending
- Lower ranks assigned to smaller nodes

**Example**:
```
node-small:  8 CPU, 32GB RAM   → capacity = 8 + 8 = 16   → rank = -1000
node-medium: 16 CPU, 64GB RAM  → capacity = 16 + 16 = 32 → rank = -999
node-large:  32 CPU, 128GB RAM → capacity = 32 + 32 = 64 → rank = -998
```

### UnallocatedVCPUPerPodCost

Ranks nodes by the ratio of unallocated vCPU to pod count, prioritizing nodes with more unused CPU per pod.

**Use Case**: Optimize for CPU efficiency by targeting nodes with the most wasted CPU capacity per pod.

**Calculation**:
- Unallocated vCPU = Allocatable CPU - Requested CPU
- Ratio = Unallocated vCPU / Pod Count
- Nodes sorted by ratio descending
- Lower ranks assigned to nodes with higher ratios

**Example**:
```
node-a: 16 CPU allocatable, 4 CPU requested, 4 pods → ratio = 12/4 = 3.0 → rank = -1000
node-b: 8 CPU allocatable, 4 CPU requested, 2 pods  → ratio = 4/2 = 2.0  → rank = -999
node-c: 8 CPU allocatable, 6 CPU requested, 4 pods  → ratio = 2/4 = 0.5  → rank = -998
```

## Do-Not-Disrupt Handling

Nodes hosting pods with the `karpenter.sh/do-not-disrupt` annotation are automatically partitioned into a separate group and assigned higher ranks (lower deletion priority).

**Ranking Process**:
1. Partition nodes into two groups:
   - **Group A**: Nodes without do-not-disrupt pods
   - **Group B**: Nodes with do-not-disrupt pods
2. Apply the configured ranking strategy within each group
3. Assign ranks to Group A starting from -1000
4. Assign ranks to Group B starting from Group A's max rank + 1

**Example**:
```
Group A (no do-not-disrupt):
  node-1 → rank = -1000
  node-2 → rank = -999
  node-3 → rank = -998

Group B (has do-not-disrupt):
  node-4 → rank = -997
  node-5 → rank = -996
```

This ensures that pods on nodes with critical workloads are always deprioritized for deletion.

## Annotation Management

### Karpenter-Managed Annotations

When Karpenter sets the deletion cost, it adds two annotations to each pod:

```yaml
annotations:
  controller.kubernetes.io/pod-deletion-cost: "-1000"
  karpenter.sh/managed-deletion-cost: "true"
```

The `karpenter.sh/managed-deletion-cost` annotation tracks which pods are managed by Karpenter.

### Customer-Managed Annotations

If a pod already has the `controller.kubernetes.io/pod-deletion-cost` annotation but lacks the `karpenter.sh/managed-deletion-cost` annotation, Karpenter will **not** modify it. This protects customer-set deletion costs.

**Example**:
```yaml
# This pod's deletion cost will NOT be modified by Karpenter
apiVersion: v1
kind: Pod
metadata:
  annotations:
    controller.kubernetes.io/pod-deletion-cost: "100"
```

### Annotation Updates

Karpenter updates pod annotations during reconciliation when:
- New nodes are added to the cluster
- Nodes are removed from the cluster
- Pod assignments change
- The ranking strategy is modified

Updates are batched for efficiency and handle errors gracefully (e.g., pod deletion during update).

## Change Detection Optimization

When `POD_DELETION_COST_CHANGE_DETECTION` is enabled (default), Karpenter computes a hash of the cluster state (nodes and pod assignments) and skips ranking computation if nothing has changed since the last reconciliation.

**Benefits**:
- Reduces unnecessary API calls
- Improves controller performance
- Lowers CPU usage during steady-state

**When to Disable**:
- Debugging ranking behavior
- Testing different strategies
- Troubleshooting annotation issues

## Usage Examples

### Example 1: Cost Optimization

Prioritize terminating pods on larger, more expensive instances:

```yaml
env:
  - name: POD_DELETION_COST_ENABLED
    value: "true"
  - name: POD_DELETION_COST_RANKING_STRATEGY
    value: "LargestToSmallest"
```

### Example 2: Consolidation

Prioritize terminating pods on smaller instances to consolidate workloads:

```yaml
env:
  - name: POD_DELETION_COST_ENABLED
    value: "true"
  - name: POD_DELETION_COST_RANKING_STRATEGY
    value: "SmallestToLargest"
```

### Example 3: CPU Efficiency

Optimize for CPU utilization by targeting nodes with the most unused CPU:

```yaml
env:
  - name: POD_DELETION_COST_ENABLED
    value: "true"
  - name: POD_DELETION_COST_RANKING_STRATEGY
    value: "UnallocatedVCPUPerPodCost"
```

### Example 4: Protecting Critical Workloads

Combine with do-not-disrupt annotations to protect critical pods:

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    karpenter.sh/do-not-disrupt: "true"
spec:
  # ... pod spec
```

Pods on nodes hosting this pod will automatically receive higher deletion costs.

## Metrics

The feature exposes the following Prometheus metrics:

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `pod_deletion_cost_nodes_ranked_total` | Counter | Number of nodes ranked | `strategy` |
| `pod_deletion_cost_pods_updated_total` | Counter | Number of pods updated | `result` (success/skipped_customer_managed/error) |
| `pod_deletion_cost_ranking_duration_seconds` | Histogram | Time to compute rankings | `strategy` |
| `pod_deletion_cost_annotation_duration_seconds` | Histogram | Time to update annotations | - |
| `pod_deletion_cost_skipped_no_changes_total` | Counter | Reconciles skipped due to no changes | - |

## Events

The controller publishes the following Kubernetes events:

| Event | Type | Description |
|-------|------|-------------|
| `PodDeletionCostRankingCompleted` | Normal | Ranking computation completed successfully |
| `PodDeletionCostUpdateFailed` | Warning | Failed to update pod annotation |
| `PodDeletionCostDisabled` | Warning | Feature disabled due to error |

## Troubleshooting

### Pods Not Receiving Deletion Cost Annotations

**Symptoms**: Pods on Karpenter-managed nodes don't have the `controller.kubernetes.io/pod-deletion-cost` annotation.

**Possible Causes**:
1. Feature is disabled
   - **Solution**: Set `POD_DELETION_COST_ENABLED=true`
2. Pods have customer-managed deletion costs
   - **Solution**: Check for existing `controller.kubernetes.io/pod-deletion-cost` without `karpenter.sh/managed-deletion-cost`
3. Controller errors
   - **Solution**: Check controller logs for errors

### Unexpected Ranking Order

**Symptoms**: Pods are being deleted in an unexpected order.

**Possible Causes**:
1. Wrong ranking strategy configured
   - **Solution**: Verify `POD_DELETION_COST_RANKING_STRATEGY` value
2. Do-not-disrupt pods affecting rankings
   - **Solution**: Check for `karpenter.sh/do-not-disrupt` annotations
3. Multiple controllers managing deletion costs
   - **Solution**: Ensure only one controller is managing the annotation

### High API Server Load

**Symptoms**: Increased API server requests from Karpenter.

**Possible Causes**:
1. Change detection disabled
   - **Solution**: Enable `POD_DELETION_COST_CHANGE_DETECTION=true`
2. Frequent cluster state changes
   - **Solution**: This is expected behavior; consider adjusting reconciliation frequency

### Annotations Not Updating After Configuration Change

**Symptoms**: Deletion cost values don't reflect the new ranking strategy.

**Possible Causes**:
1. Change detection preventing updates
   - **Solution**: Temporarily disable change detection or trigger a node/pod change
2. Controller not restarted after configuration change
   - **Solution**: Restart the Karpenter controller

## RBAC Requirements

The Pod Deletion Cost controller requires the following RBAC permissions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: karpenter
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
```

## Limitations

1. **Kubernetes Version**: Requires Kubernetes 1.22+ (when pod deletion cost was introduced)
2. **Scope**: Only affects pods on Karpenter-managed nodes
3. **Timing**: Annotations are updated during reconciliation, not immediately on pod creation
4. **Conflicts**: Customer-set deletion costs are respected and not overwritten
5. **Scale**: Performance tested up to 1000 nodes; larger clusters may require tuning

## Best Practices

1. **Start with Random**: Test the feature with the `Random` strategy before using optimization strategies
2. **Monitor Metrics**: Track `pod_deletion_cost_pods_updated_total` to ensure annotations are being applied
3. **Use Do-Not-Disrupt**: Combine with `karpenter.sh/do-not-disrupt` for critical workloads
4. **Enable Change Detection**: Keep `POD_DELETION_COST_CHANGE_DETECTION=true` for production
5. **Test Strategy Changes**: Validate ranking behavior in non-production before changing strategies
6. **Respect Customer Annotations**: Don't manually add `karpenter.sh/managed-deletion-cost` to pods

## References

- [Kubernetes Pod Deletion Cost](https://kubernetes.io/docs/concepts/workloads/controllers/replicaset/#pod-deletion-cost)
- [Karpenter Disruption Controls](https://karpenter.sh/docs/concepts/disruption/)
- [Pod Deletion Cost Algorithm](https://rpadovani.com/k8s-algorithm-pick-pod-scale-in)
