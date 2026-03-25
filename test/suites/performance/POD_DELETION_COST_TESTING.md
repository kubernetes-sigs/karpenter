# Pod Deletion Cost Performance Testing

This document describes how to configure and test the Pod Deletion Cost Management feature in Karpenter performance tests.

## Overview

The Pod Deletion Cost Management feature allows Karpenter to automatically manage the `controller.kubernetes.io/pod-deletion-cost` annotation on pods. This annotation influences which pods Kubernetes terminates first during scale-in events.

## Configuration Variables

The performance test suite includes three configuration variables for pod deletion cost testing:

### 1. `podDeletionCostEnabled` (bool)
- **Default**: `false`
- **Description**: Tracks whether the pod deletion cost management feature is enabled in the Karpenter deployment being tested
- **Important**: This is for reporting only. To actually enable the feature, set `FEATURE_GATES="PodDeletionCostManagement=true"` in your Karpenter deployment
- **Values**: 
  - `true`: Feature is enabled in deployment
  - `false`: Feature is disabled in deployment

### 2. `podDeletionCostRankingStrategy` (string)
- **Default**: `"Random"`
- **Description**: Tracks the ranking strategy configured in the Karpenter deployment
- **Important**: This is for reporting only. To configure the strategy, set `POD_DELETION_COST_RANKING_STRATEGY` in your Karpenter deployment
- **Valid Values**:
  - `"Random"`: Nodes are ranked randomly
  - `"LargestToSmallest"`: Larger nodes (by capacity) get lower deletion cost (deleted first)
  - `"SmallestToLargest"`: Smaller nodes get lower deletion cost (deleted first)
  - `"UnallocatedVCPUPerPodCost"`: Nodes with higher unallocated vCPU per pod ratio get lower deletion cost

### 3. `podDeletionCostChangeDetection` (bool)
- **Default**: `true`
- **Description**: Tracks whether change detection optimization is enabled in the Karpenter deployment
- **Important**: This is for reporting only. To configure change detection, set `POD_DELETION_COST_CHANGE_DETECTION` in your Karpenter deployment
- **Values**:
  - `true`: Skip ranking when no changes detected (more efficient)
  - `false`: Always perform ranking on every reconcile

## Important: Reporting vs Control

⚠️ **These variables are for REPORTING purposes only!**

The performance tests run against an already-deployed Karpenter instance. These variables should be set to **match** your Karpenter deployment configuration so that performance reports accurately reflect what was tested.

To actually enable and configure the feature:
1. Set environment variables in your Karpenter deployment
2. Update these test variables to match
3. Run tests
4. Reports will include your configuration

See [POD_DELETION_COST_SETUP.md](POD_DELETION_COST_SETUP.md) for detailed setup instructions.

## How to Configure

### Method 1: Edit suite_test.go

Edit `test/suites/performance/suite_test.go` and modify the variables at the top:

```go
// Enable pod deletion cost with UnallocatedVCPUPerPodCost strategy
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "UnallocatedVCPUPerPodCost"
var podDeletionCostChangeDetection bool = true
```

### Method 2: Environment Variables (Future Enhancement)

In the future, these could be exposed as environment variables:
```bash
export POD_DELETION_COST_ENABLED=true
export POD_DELETION_COST_RANKING_STRATEGY=UnallocatedVCPUPerPodCost
export POD_DELETION_COST_CHANGE_DETECTION=true
```

## Running Tests

### Run All Performance Tests with Pod Deletion Cost Enabled

1. Edit `suite_test.go` to enable the feature
2. Run the tests:
```bash
go test -v ./test/suites/performance
```

### Run Specific Test

```bash
go test -v ./test/suites/performance -run TestIntegration/Performance/BasicDeployment
```

### Run Example Test

The example test demonstrates the configuration:
```bash
# First, remove the Skip() call in pod_deletion_cost_example_test.go
go test -v ./test/suites/performance -run TestIntegration/Performance/PodDeletionCostExample
```

## Performance Report Output

When pod deletion cost is enabled, the performance reports will include:

### Console Output
```
=== CONSOLIDATION PERFORMANCE REPORT ===
Test: Consolidation Test
Type: consolidation
Total Time: 5m30s
Total Pods: 400 (Net Change: -200)
Total Nodes: 15 (Net Change: -5)
Pods Disrupted: 200
CPU Utilization: 65.50%
Memory Utilization: 72.30%
Efficiency Score: 66.2%
Pods per Node: 26.7
Rounds: 3
Size Class Lock Threshold: disabled
Pod Deletion Cost: enabled
  Ranking Strategy: UnallocatedVCPUPerPodCost
  Change Detection: true
```

### JSON Output
When `OUTPUT_DIR` environment variable is set, reports are saved as JSON:

```json
{
  "test_name": "Consolidation Test",
  "test_type": "consolidation",
  "total_pods": 400,
  "total_nodes": 15,
  "total_time": 330000000000,
  "change_in_pod_count": -200,
  "change_in_node_count": -5,
  "pods_disrupted": 200,
  "total_reserved_cpu_utilization": 0.655,
  "total_reserved_memory_utilization": 0.723,
  "resource_efficiency_score": 66.2,
  "pods_per_node": 26.7,
  "rounds": 3,
  "size_class_lock_threshold": 0,
  "pod_deletion_cost_enabled": true,
  "pod_deletion_cost_ranking_strategy": "UnallocatedVCPUPerPodCost",
  "pod_deletion_cost_change_detection": true,
  "timestamp": "2024-01-15T10:30:00Z"
}
```

## Testing Different Strategies

### Test Random Strategy
```go
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "Random"
```
**Expected Behavior**: Pods should be disrupted in random order, no particular pattern.

### Test LargestToSmallest Strategy
```go
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "LargestToSmallest"
```
**Expected Behavior**: Pods on larger nodes should be disrupted first during consolidation.

### Test SmallestToLargest Strategy
```go
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "SmallestToLargest"
```
**Expected Behavior**: Pods on smaller nodes should be disrupted first during consolidation.

### Test UnallocatedVCPUPerPodCost Strategy
```go
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "UnallocatedVCPUPerPodCost"
```
**Expected Behavior**: Nodes with more unallocated vCPU per pod should have their pods disrupted first.

## Comparing Results

To compare performance with and without pod deletion cost:

1. Run tests with feature disabled:
```go
var podDeletionCostEnabled bool = false
```

2. Run tests with feature enabled:
```go
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "UnallocatedVCPUPerPodCost"
```

3. Compare the JSON reports:
   - Consolidation time
   - Number of rounds
   - Resource utilization
   - Pods disrupted

## Troubleshooting

### Feature Not Working

1. Verify the feature gate is enabled in Karpenter deployment
2. Check Karpenter logs for pod deletion cost controller activity
3. Verify pods have the `controller.kubernetes.io/pod-deletion-cost` annotation

### Unexpected Behavior

1. Check the ranking strategy is spelled correctly
2. Verify nodes have the expected characteristics (capacity, pod counts)
3. Look for do-not-disrupt annotations that might affect ranking

## Integration with Size Class Locking

Both features can be enabled simultaneously:

```go
var sizeClassLockThreshold int = 10
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "UnallocatedVCPUPerPodCost"
```

This allows testing the interaction between:
- Size class locking (affects node selection during provisioning)
- Pod deletion cost (affects pod disruption order during consolidation)

## Future Enhancements

Potential improvements to the testing framework:

1. Add metrics collection for pod deletion cost controller performance
2. Add validation that pods have correct deletion cost annotations
3. Add verification that disruption order matches expected strategy
4. Add comparative analysis between different strategies
5. Add support for environment variable configuration
