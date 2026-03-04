# Pod Deletion Cost Controller

This package implements the Pod Deletion Cost Management feature for Karpenter, which automatically manages the `controller.kubernetes.io/pod-deletion-cost` annotation on pods to influence Kubernetes' pod selection during scale-in events.

## Overview

The Pod Deletion Cost Management feature consists of four main components:

1. **Ranking Engine** - Ranks nodes based on configurable strategies
2. **Annotation Manager** - Updates pod deletion cost annotations safely
3. **Change Detector** - Optimizes by detecting when ranking is needed
4. **Controller** - Orchestrates the workflow

## Components

### Ranking Engine (`ranking.go`)
Implements different strategies for ranking nodes:
- **Random**: Random ordering
- **LargestToSmallest**: Larger nodes deleted first
- **SmallestToLargest**: Smaller nodes deleted first
- **UnallocatedVCPUPerPodCost**: Nodes with more unallocated vCPU per pod deleted first

Special handling for do-not-disrupt pods: nodes hosting these pods are ranked separately and given higher deletion costs.

### Annotation Manager (`annotation.go`)
Safely updates pod deletion cost annotations:
- Adds annotations to new pods
- Updates Karpenter-managed pods
- Protects customer-set annotations
- Adds management tracking annotation

### Change Detector (`changedetector.go`)
Optimizes performance by detecting changes:
- Tracks node and pod state via hashing
- Skips ranking when no changes detected
- Configurable optimization

### Controller (`controller.go`)
Orchestrates the complete workflow:
- Watches Karpenter-managed nodes and pods
- Triggers ranking on changes
- Updates pod annotations
- Publishes metrics and events

## Configuration

### Feature Gate
```go
PodDeletionCostManagement: true/false
```

### Environment Variables
```bash
POD_DELETION_COST_ENABLED=true
POD_DELETION_COST_RANKING_STRATEGY=UnallocatedVCPUPerPodCost
POD_DELETION_COST_CHANGE_DETECTION=true
```

### Ranking Strategies
- `Random` - Random ordering
- `LargestToSmallest` - Prefer deleting pods on larger nodes
- `SmallestToLargest` - Prefer deleting pods on smaller nodes
- `UnallocatedVCPUPerPodCost` - Prefer nodes with more unallocated vCPU per pod

## Usage

### Basic Usage
```go
// Create ranking engine
rankingEngine := deletioncost.NewRankingEngine(deletioncost.RankingStrategyLargestToSmallest)

// Rank nodes
ranks, err := rankingEngine.RankNodes(ctx, nodes)

// Update pod annotations
annotationMgr := deletioncost.NewAnnotationManager(kubeClient)
err = annotationMgr.UpdatePodDeletionCosts(ctx, ranks)
```

### With Change Detection
```go
changeDetector := deletioncost.NewChangeDetector()

if changeDetector.HasChanged(nodes) {
    ranks, err := rankingEngine.RankNodes(ctx, nodes)
    err = annotationMgr.UpdatePodDeletionCosts(ctx, ranks)
}
```

## Annotations

### Pod Deletion Cost
```yaml
controller.kubernetes.io/pod-deletion-cost: "100"
```
Lower values are deleted first. Range: negative to positive integers.

### Karpenter Management Tracking
```yaml
karpenter.sh/managed-deletion-cost: "true"
```
Indicates Karpenter is managing the deletion cost for this pod.

## Testing

Comprehensive test suite with 55+ test cases covering:
- All ranking strategies
- Annotation updates and protection
- Change detection
- Do-not-disrupt handling
- Error handling
- Edge cases
- Performance scenarios

### Run Tests
```bash
# All tests
go test ./pkg/controllers/pod/deletioncost/...

# With coverage
go test -cover ./pkg/controllers/pod/deletioncost/...

# Specific component
go test ./pkg/controllers/pod/deletioncost -run TestRanking
```

See [QUICKSTART_TESTING.md](QUICKSTART_TESTING.md) for quick start guide.  
See [TESTING.md](TESTING.md) for comprehensive testing documentation.  
See [TEST_SUMMARY.md](TEST_SUMMARY.md) for test coverage summary.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Pod Deletion Cost Controller              │
├─────────────────────────────────────────────────────────────┤
│                                                               │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐  │
│  │   Ranking    │───▶│  Annotation  │───▶│   Metrics    │  │
│  │   Engine     │    │   Manager    │    │   & Events   │  │
│  └──────────────┘    └──────────────┘    └──────────────┘  │
│         ▲                                                     │
│         │                                                     │
│  ┌──────────────┐                                            │
│  │    Change    │                                            │
│  │   Detector   │                                            │
│  └──────────────┘                                            │
│         ▲                                                     │
│         │                                                     │
│  ┌──────────────────────────────────────────────────────┐   │
│  │              State Cluster (Nodes & Pods)            │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

## Workflow

1. **Watch** - Controller watches for node/pod changes
2. **Detect** - Change detector determines if ranking is needed
3. **Rank** - Ranking engine ranks nodes based on strategy
4. **Partition** - Nodes with do-not-disrupt pods ranked separately
5. **Update** - Annotation manager updates pod deletion costs
6. **Protect** - Customer-set annotations are preserved
7. **Track** - Management annotation added to Karpenter-managed pods
8. **Metrics** - Performance metrics recorded
9. **Events** - Events published for observability

## Metrics

```prometheus
# Number of nodes ranked
pod_deletion_cost_nodes_ranked_total{strategy="..."}

# Number of pods updated
pod_deletion_cost_pods_updated_total{result="success|skipped|error"}

# Ranking duration
pod_deletion_cost_ranking_duration_seconds{strategy="..."}

# Annotation update duration
pod_deletion_cost_annotation_duration_seconds

# Change detection skips
pod_deletion_cost_skipped_no_changes_total
```

## Events

```go
// Ranking completed
PodDeletionCostRankingCompleted(nodeCount, strategy)

// Annotation update failed
PodDeletionCostUpdateFailed(pod, error)

// Feature disabled
PodDeletionCostDisabled(reason)
```

## Do-Not-Disrupt Handling

Nodes hosting pods with `karpenter.sh/do-not-disrupt: "true"` annotation are:
1. Identified and partitioned separately
2. Ranked within their own group
3. Assigned higher deletion costs than normal nodes
4. Protected from early deletion during scale-in

## Customer Annotation Protection

Pods with existing `controller.kubernetes.io/pod-deletion-cost` annotation but without `karpenter.sh/managed-deletion-cost` are considered customer-managed and will NOT be modified by Karpenter.

## Performance

- **Change Detection**: O(n) hash computation, skips O(n log n) ranking when no changes
- **Ranking**: O(n log n) for sorting-based strategies, O(n) for random
- **Annotation Updates**: Batched to reduce API calls
- **Tested**: Handles 1000+ nodes and 100+ pods per node efficiently

## Error Handling

- **Transient Errors**: Pod not found, conflict on update → Continue with other pods
- **Configuration Errors**: Invalid strategy → Log error, disable feature
- **API Errors**: Unable to list nodes/pods → Requeue with backoff

## Security

### RBAC Permissions Required
```yaml
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch", "update", "patch"]
  
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch"]
```

## Documentation

- **Design**: `.kiro/specs/pod-deletion-cost-management/design.md`
- **Requirements**: `.kiro/specs/pod-deletion-cost-management/requirements.md`
- **Tasks**: `.kiro/specs/pod-deletion-cost-management/tasks.md`
- **Testing Quick Start**: `QUICKSTART_TESTING.md`
- **Testing Guide**: `TESTING.md`
- **Test Summary**: `TEST_SUMMARY.md`

## Examples

### Example 1: Basic Setup
```go
// Initialize components
rankingEngine := deletioncost.NewRankingEngine(deletioncost.RankingStrategyLargestToSmallest)
annotationMgr := deletioncost.NewAnnotationManager(kubeClient)

// Get nodes from cluster state
nodes := cluster.Nodes()

// Rank and update
ranks, _ := rankingEngine.RankNodes(ctx, nodes)
annotationMgr.UpdatePodDeletionCosts(ctx, ranks)
```

### Example 2: With Change Detection
```go
changeDetector := deletioncost.NewChangeDetector()

// In reconcile loop
if changeDetector.HasChanged(nodes) {
    ranks, _ := rankingEngine.RankNodes(ctx, nodes)
    annotationMgr.UpdatePodDeletionCosts(ctx, ranks)
}
```

### Example 3: Different Strategies
```go
// Random
random := deletioncost.NewRankingEngine(deletioncost.RankingStrategyRandom)

// Largest first
largestFirst := deletioncost.NewRankingEngine(deletioncost.RankingStrategyLargestToSmallest)

// Smallest first
smallestFirst := deletioncost.NewRankingEngine(deletioncost.RankingStrategySmallestToLargest)

// Unallocated vCPU
vcpuBased := deletioncost.NewRankingEngine(deletioncost.RankingStrategyUnallocatedVCPUPerPodCost)
```

## Troubleshooting

### Feature Not Working
1. Check feature gate is enabled
2. Verify RBAC permissions
3. Check controller logs
4. Verify pods have annotations

### Unexpected Ranking
1. Check ranking strategy configuration
2. Verify node characteristics (capacity, pod counts)
3. Check for do-not-disrupt annotations
4. Review controller logs

### Performance Issues
1. Enable change detection
2. Check node/pod count
3. Review metrics
4. Consider adjusting reconcile frequency

## Contributing

When contributing to this package:
1. Follow existing code patterns
2. Add tests for new functionality
3. Update documentation
4. Maintain >90% test coverage
5. Run tests before submitting: `go test ./pkg/controllers/pod/deletioncost/...`

## License

Copyright The Kubernetes Authors. Licensed under Apache License 2.0.
