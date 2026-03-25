# Pod Deletion Cost Performance Testing Setup

## Overview

The performance test suite can track and report pod deletion cost settings, but it **does not control** the Karpenter deployment's feature gates. You must enable the feature in your Karpenter deployment separately.

## Two-Step Setup

### Step 1: Enable Feature in Karpenter Deployment

Set the feature gate and configuration in your Karpenter deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: karpenter
  namespace: karpenter
spec:
  template:
    spec:
      containers:
      - name: controller
        env:
          # Enable pod deletion cost management
          - name: FEATURE_GATES
            value: "PodDeletionCostManagement=true"
          
          # Configure ranking strategy
          - name: POD_DELETION_COST_RANKING_STRATEGY
            value: "UnallocatedVCPUPerPodCost"
          
          # Enable change detection
          - name: POD_DELETION_COST_CHANGE_DETECTION
            value: "true"
```

Or via Helm:

```yaml
# values.yaml
controller:
  env:
    - name: FEATURE_GATES
      value: "PodDeletionCostManagement=true"
    - name: POD_DELETION_COST_RANKING_STRATEGY
      value: "UnallocatedVCPUPerPodCost"
    - name: POD_DELETION_COST_CHANGE_DETECTION
      value: "true"
```

### Step 2: Update Test Variables to Match

Edit `test/suites/performance/suite_test.go` to match your deployment:

```go
// Set these to match your Karpenter deployment configuration
var podDeletionCostEnabled bool = true  // Match your FEATURE_GATES setting
var podDeletionCostRankingStrategy string = "UnallocatedVCPUPerPodCost"  // Match your strategy
var podDeletionCostChangeDetection bool = true  // Match your change detection setting
```

## Why This Design?

The performance tests run against an **already-deployed** Karpenter instance:

1. **Karpenter is deployed** with feature gates set via environment variables
2. **Tests run** against that deployment
3. **Test variables** document what configuration was tested
4. **Reports include** the configuration for comparison

This allows you to:
- Test different configurations by redeploying Karpenter
- Compare performance reports with different settings
- Track which configuration produced which results

## Testing Different Configurations

### Test 1: Baseline (Feature Disabled)

**Karpenter Deployment**:
```yaml
env:
  - name: FEATURE_GATES
    value: "PodDeletionCostManagement=false"
```

**Test Variables**:
```go
var podDeletionCostEnabled bool = false
```

**Run Tests**:
```bash
go test ./test/suites/performance/... -v
```

### Test 2: Random Strategy

**Karpenter Deployment**:
```yaml
env:
  - name: FEATURE_GATES
    value: "PodDeletionCostManagement=true"
  - name: POD_DELETION_COST_RANKING_STRATEGY
    value: "Random"
```

**Test Variables**:
```go
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "Random"
```

**Run Tests**:
```bash
go test ./test/suites/performance/... -v
```

### Test 3: UnallocatedVCPUPerPodCost Strategy

**Karpenter Deployment**:
```yaml
env:
  - name: FEATURE_GATES
    value: "PodDeletionCostManagement=true"
  - name: POD_DELETION_COST_RANKING_STRATEGY
    value: "UnallocatedVCPUPerPodCost"
```

**Test Variables**:
```go
var podDeletionCostEnabled bool = true
var podDeletionCostRankingStrategy string = "UnallocatedVCPUPerPodCost"
```

**Run Tests**:
```bash
go test ./test/suites/performance/... -v
```

## Comparing Results

After running tests with different configurations, compare the JSON reports:

```bash
# Baseline
cat $OUTPUT_DIR/baseline_consolidation_performance_report.json

# Random strategy
cat $OUTPUT_DIR/random_consolidation_performance_report.json

# UnallocatedVCPU strategy
cat $OUTPUT_DIR/vcpu_consolidation_performance_report.json
```

Each report will include:
```json
{
  "pod_deletion_cost_enabled": true,
  "pod_deletion_cost_ranking_strategy": "UnallocatedVCPUPerPodCost",
  "pod_deletion_cost_change_detection": true,
  "total_time": "5m30s",
  "pods_disrupted": 200,
  ...
}
```

## Verification

### Verify Feature is Enabled in Karpenter

```bash
# Check Karpenter logs
kubectl logs -n karpenter deployment/karpenter | grep "PodDeletionCostManagement"

# Check controller is running
kubectl logs -n karpenter deployment/karpenter | grep "pod.deletioncost"

# Check pod annotations
kubectl get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.controller\.kubernetes\.io/pod-deletion-cost}{"\n"}{end}'
```

### Verify Test Variables Match

Check that your test variables in `suite_test.go` match your deployment:

```bash
# What's in your deployment?
kubectl get deployment -n karpenter karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FEATURE_GATES")].value}'

# What's in your tests?
grep "podDeletionCostEnabled" test/suites/performance/suite_test.go
```

## Common Mistakes

### ❌ Mistake 1: Setting Test Variable But Not Feature Gate

```go
// In test
var podDeletionCostEnabled bool = true  // ✅ Set

// In Karpenter deployment
FEATURE_GATES="PodDeletionCostManagement=false"  // ❌ Not enabled!
```

**Result**: Tests report feature as enabled, but it's actually disabled. Reports are misleading.

### ❌ Mistake 2: Enabling Feature But Not Updating Test Variable

```yaml
# In Karpenter deployment
env:
  - name: FEATURE_GATES
    value: "PodDeletionCostManagement=true"  # ✅ Enabled
```

```go
// In test
var podDeletionCostEnabled bool = false  // ❌ Not updated!
```

**Result**: Feature is working, but reports show it as disabled. Reports are misleading.

### ✅ Correct: Both Match

```yaml
# In Karpenter deployment
env:
  - name: FEATURE_GATES
    value: "PodDeletionCostManagement=true"  # ✅ Enabled
```

```go
// In test
var podDeletionCostEnabled bool = true  # ✅ Matches!
```

**Result**: Feature is enabled and reports accurately reflect the configuration.

## Automated Testing Script

Here's a script to test all configurations:

```bash
#!/bin/bash

# Test configurations
configs=(
  "false:Random:true"
  "true:Random:true"
  "true:LargestToSmallest:true"
  "true:SmallestToLargest:true"
  "true:UnallocatedVCPUPerPodCost:true"
)

for config in "${configs[@]}"; do
  IFS=':' read -r enabled strategy detection <<< "$config"
  
  echo "Testing: enabled=$enabled, strategy=$strategy, detection=$detection"
  
  # Update Karpenter deployment
  kubectl set env deployment/karpenter -n karpenter \
    FEATURE_GATES="PodDeletionCostManagement=$enabled" \
    POD_DELETION_COST_RANKING_STRATEGY="$strategy" \
    POD_DELETION_COST_CHANGE_DETECTION="$detection"
  
  # Wait for rollout
  kubectl rollout status deployment/karpenter -n karpenter
  
  # Update test variables
  sed -i "s/var podDeletionCostEnabled bool = .*/var podDeletionCostEnabled bool = $enabled/" test/suites/performance/suite_test.go
  sed -i "s/var podDeletionCostRankingStrategy string = .*/var podDeletionCostRankingStrategy string = \"$strategy\"/" test/suites/performance/suite_test.go
  sed -i "s/var podDeletionCostChangeDetection bool = .*/var podDeletionCostChangeDetection bool = $detection/" test/suites/performance/suite_test.go
  
  # Run tests
  OUTPUT_DIR="./results/${enabled}_${strategy}_${detection}" go test ./test/suites/performance/... -v
  
  echo "Results saved to ./results/${enabled}_${strategy}_${detection}"
done
```

## Summary

✅ **Step 1**: Enable feature in Karpenter deployment (environment variables)  
✅ **Step 2**: Update test variables to match (for accurate reporting)  
✅ **Step 3**: Run tests and compare results  

The test variables are for **reporting**, not **control**. Always configure the feature in your Karpenter deployment first!
