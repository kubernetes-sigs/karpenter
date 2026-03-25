# Pod Deletion Cost Deployment & Testing Summary

## Overview
Successfully implemented deployment tooling and testing enhancements for the Pod Deletion Cost Management feature in Karpenter.

## Changes Made

### 1. Deployment Script (`scripts/deploy-karpenter.sh`)
Created a comprehensive deployment script with the following features:
- Configurable pod deletion cost settings via command-line flags
- Support for all ranking strategies (Random, LargestToSmallest, SmallestToLargest, UnallocatedVCPUPerPodCost)
- Change detection toggle
- Skip build option for faster redeployment
- Clear output and verification instructions

**Usage:**
```bash
# Deploy with pod deletion cost enabled
./scripts/deploy-karpenter.sh --enable-pod-deletion-cost --ranking-strategy UnallocatedVCPUPerPodCost

# Deploy with custom settings
./scripts/deploy-karpenter.sh \
  --enable-pod-deletion-cost \
  --ranking-strategy SmallestToLargest \
  --change-detection false
```

### 2. Makefile Updates
Enhanced the `apply` target to support pod deletion cost configuration via environment variables:

```bash
# Using make with environment variables
POD_DELETION_COST_ENABLED=true \
POD_DELETION_COST_RANKING_STRATEGY=UnallocatedVCPUPerPodCost \
make apply
```

### 3. Controller Reconciliation Fix (`pkg/controllers/pod/deletioncost/controller.go`)
**Problem:** Controller was using singleton source and not reconciling automatically.

**Solution:** Changed to periodic reconciliation every 1 minute:
- Added `const reconcileInterval = time.Minute`
- Updated all `Reconcile()` return statements to include `RequeueAfter: reconcileInterval`
- Controller now runs automatically without manual triggers

### 4. Performance Test Enhancements (`test/suites/performance/basic_test.go`)
Added pod deletion cost verification between scale-out and consolidation phases:

**Features:**
- Checks for pod deletion cost annotations after scale-out completes
- Waits 1 minute and retries if annotations not detected initially
- Tracks and reports disruptions during the wait period
- **Consolidation report automatically excludes wait-period disruptions** (captures its own baseline)
- Provides clear reporting of wait-period disruptions for visibility
- Continues test regardless of annotation detection status

**Output Examples:**
```
✓ Pod deletion cost annotations detected and working
Pod deletion cost check: 1000/1000 pods have annotations (100.0%)

⚠ 151 pod disruptions occurred during deletion cost check wait period
Note: 151 disruptions occurred during wait period (automatically excluded from consolidation report)
Consolidation disruptions: 300
```

### 4.1 Consolidation Report Updates (`test/suites/performance/report.go`)
Modified `ReportConsolidation()` and `ReportConsolidationWithOutput()` functions:
- Added `baselineDisruptions` parameter for flexibility (typically set to 0)
- Function captures its own baseline at start, automatically excluding prior disruptions
- Updated documentation to clarify when to use baselineDisruptions parameter
- All performance tests pass `0` for baseline disruptions (standard behavior)

**Important Note:** The consolidation report function captures a disruption baseline when it starts, so any disruptions that occurred before calling the function (including during wait periods) are automatically excluded. The `baselineDisruptions` parameter is kept for API flexibility but is typically 0.

### 5. Helper Function (`test/suites/performance/suite_test.go`)
Added `checkPodDeletionCostAnnotations()` function:
- Lists all application pods (excludes system namespaces)
- Checks for `controller.kubernetes.io/pod-deletion-cost` annotation
- Reports percentage of annotated pods
- Returns true if ≥50% of pods have annotations

### 6. Variable Declarations (`test/suites/performance/types.go`)
Moved pod deletion cost configuration variables to types.go for proper compilation order:
- `podDeletionCostEnabled`
- `podDeletionCostRankingStrategy`
- `podDeletionCostChangeDetection`

Variables are initialized in `suite_test.go` init() function from environment variables.

## Test Workload
Created `test-workload.yaml` for manual testing:
- 5 pods requesting 1 vCPU and 2GB memory each
- Targets 'default' Karpenter nodepool
- Uses pause container for minimal resource usage

**Usage:**
```bash
kubectl apply -f test-workload.yaml
kubectl get pods -l app=test-pod-deletion-cost -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.controller\.kubernetes\.io/pod-deletion-cost}{"\n"}{end}'
```

## Verification

### Controller Status
```bash
# Check controller is running
kubectl logs -n karpenter deployment/karpenter | grep "pod.deletioncost"

# Check environment variables
kubectl get deployment -n karpenter karpenter -o jsonpath='{.spec.template.spec.containers[0].env}' | jq .
```

### Pod Annotations
```bash
# Check specific pod
kubectl get pod <pod-name> -o yaml | grep -A3 "annotations:"

# Check all pods
kubectl get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.controller\.kubernetes\.io/pod-deletion-cost}{"\t"}{.metadata.annotations.karpenter\.sh/managed-deletion-cost}{"\n"}{end}'
```

### Metrics
```bash
kubectl port-forward -n karpenter deployment/karpenter 8080:8080 &
curl localhost:8080/metrics | grep pod_deletion_cost
```

**Key Metrics:**
- `karpenter_pod_deletion_cost_nodes_ranked_total` - Nodes ranked by strategy
- `karpenter_pod_deletion_cost_pods_updated_total` - Pods updated (success/skipped/error)
- `karpenter_pod_deletion_cost_ranking_duration_seconds` - Ranking performance
- `karpenter_pod_deletion_cost_annotation_duration_seconds` - Annotation update performance

## Deletion Cost Values

Pod deletion costs are **negative** by design:
- Start at `BaseRank = -1000`
- Lower values = deleted first (higher priority for deletion)
- Higher values = deleted last (lower priority for deletion)

**Example:**
- Pods with -997: Deleted first
- Pods with -991: Deleted later
- Pods with do-not-disrupt: Get higher values (closer to 0)

This ensures:
1. Karpenter-managed pods have lower deletion costs than default (0)
2. Room for customer-set positive values to take precedence
3. Do-not-disrupt pods are protected

## Configuration Options

### Feature Gate
```yaml
env:
  - name: FEATURE_GATES
    value: "PodDeletionCostManagement=true"
```

### Ranking Strategies
- **Random**: Random ordering (good for testing)
- **LargestToSmallest**: Larger nodes deleted first (consolidate to smaller nodes)
- **SmallestToLargest**: Smaller nodes deleted first (keep larger nodes)
- **UnallocatedVCPUPerPodCost**: Nodes with more unallocated vCPU per pod deleted first (recommended)

### Change Detection
```yaml
env:
  - name: POD_DELETION_COST_CHANGE_DETECTION
    value: "true"
```
Enables optimization to skip ranking when cluster state hasn't changed.

## Files Modified

1. `scripts/deploy-karpenter.sh` - New deployment script
2. `Makefile` - Updated apply target with pod deletion cost support
3. `pkg/controllers/pod/deletioncost/controller.go` - Fixed reconciliation timing
4. `test/suites/performance/basic_test.go` - Added deletion cost checks and disruption tracking
5. `test/suites/performance/report.go` - Updated consolidation reporting to exclude baseline disruptions
6. `test/suites/performance/suite_test.go` - Added helper function
7. `test/suites/performance/types.go` - Added variable declarations
8. `test/suites/performance/wide_deployments_test.go` - Updated function call signature
9. `test/suites/performance/host_name_spreading_xl_test.go` - Updated function call signature
10. `test/suites/performance/host_name_spreading_test.go` - Updated function call signature
11. `test/suites/performance/interference_test.go` - Updated function call signature
12. `test/suites/performance/do_no_disrupt_test.go` - Updated function call signature
13. `test-workload.yaml` - New test workload manifest

## Next Steps

1. Run performance tests with feature enabled to validate behavior
2. Monitor metrics during consolidation events
3. Compare consolidation efficiency with/without feature enabled
4. Document any performance improvements or issues observed

## Troubleshooting

### Annotations Not Appearing
1. Check feature gate is enabled: `kubectl get deployment -n karpenter karpenter -o yaml | grep FEATURE_GATES`
2. Check controller logs: `kubectl logs -n karpenter deployment/karpenter | grep deletioncost`
3. Verify controller is reconciling: Look for "updated pod deletion costs" messages
4. Wait 1 minute for next reconciliation cycle

### Controller Not Reconciling
- Controller reconciles every 1 minute automatically
- Check controller is running: `kubectl get pods -n karpenter`
- Check for errors in logs: `kubectl logs -n karpenter deployment/karpenter --tail=100`

### Build Issues
```bash
# Rebuild and redeploy
make verify build
POD_DELETION_COST_ENABLED=true make apply
```
