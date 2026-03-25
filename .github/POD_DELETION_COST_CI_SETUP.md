# Pod Deletion Cost - CI/CD Setup Guide

## Overview

This guide explains how to configure and use the Pod Deletion Cost Management feature in GitHub Actions CI/CD workflows. The feature is integrated into the test infrastructure through workflow inputs and environment variables, allowing flexible testing of different configurations.

The Pod Deletion Cost feature helps Karpenter make smarter decisions about which pods to disrupt during consolidation by assigning deletion costs based on pod resource utilization. This integration enables automated testing of the feature across different strategies and configurations.

## Workflow Inputs

The e2e workflow (`.github/workflows/e2e.yaml`) accepts the following inputs for pod deletion cost configuration:

### `pod_deletion_cost_enabled`

- **Type**: string
- **Default**: `"false"`
- **Description**: Enable or disable the pod deletion cost management feature
- **Valid Values**: `"true"` or `"false"`
- **Note**: Uses string type instead of boolean for better GitHub Actions compatibility

### `pod_deletion_cost_ranking_strategy`

- **Type**: string
- **Default**: `"Random"`
- **Description**: The strategy used to rank pods for deletion cost calculation
- **Valid Values**:
  - `"Random"` - Assigns random deletion costs (baseline/testing)
  - `"LargestToSmallest"` - Prioritizes disrupting larger pods first
  - `"SmallestToLargest"` - Prioritizes disrupting smaller pods first
  - `"UnallocatedVCPUPerPodCost"` - Uses sophisticated cost calculation based on unallocated vCPU resources

### `pod_deletion_cost_change_detection`

- **Type**: string
- **Default**: `"true"`
- **Description**: Enable change detection optimization to avoid unnecessary recalculations
- **Valid Values**: `"true"` or `"false"`
- **Note**: When enabled, deletion costs are only recalculated when pod resources change

## Environment Variables

The Makefile and test suite use these environment variables for configuration:

### `POD_DELETION_COST_ENABLED`

- **Default**: `"false"`
- **Description**: Controls the feature gate `PodDeletionCostManagement`
- **Used By**: Makefile (`apply-with-kind` target), Karpenter controller

### `POD_DELETION_COST_RANKING_STRATEGY`

- **Default**: `"Random"`
- **Description**: Sets the ranking strategy for pod deletion cost calculation
- **Used By**: Makefile, Karpenter controller, test suite

### `POD_DELETION_COST_CHANGE_DETECTION`

- **Default**: `"true"`
- **Description**: Enables or disables change detection optimization
- **Used By**: Makefile, Karpenter controller, test suite

## Configuration by Environment

Different test environments use different default configurations:

| Environment | Enabled | Strategy | Change Detection | Rationale |
|------------|---------|----------|------------------|-----------|
| **E2E Tests** | `false` | `Random` | `true` | Baseline testing without feature |
| **Performance Tests** | `true` | `UnallocatedVCPUPerPodCost` | `true` | Validate feature performance impact |
| **Matrix Tests** | varies | varies | varies | Compare different configurations |
| **Local Development** | `false` | `Random` | `true` | Match e2e defaults |


## Usage Examples

### Example 1: Performance Tests (Default Configuration)

Performance tests automatically enable the pod deletion cost feature with the most sophisticated strategy:

```yaml
# .github/workflows/kind-perf-e2e.yaml
uses: ./.github/workflows/e2e.yaml
with:
  suite: Performance
  focus: "Basic Deployment"
  test_name: "perf-basic"
  pod_deletion_cost_enabled: "true"
  pod_deletion_cost_ranking_strategy: "UnallocatedVCPUPerPodCost"
  pod_deletion_cost_change_detection: "true"
```

This configuration:
- Enables the feature for performance validation
- Uses the most sophisticated ranking strategy
- Enables change detection for realistic performance testing
- Generates performance reports with configuration details

### Example 2: E2E Tests with Feature Enabled

To run regular e2e tests with the feature enabled (for functional validation):

```yaml
# Custom workflow or manual trigger
uses: ./.github/workflows/e2e.yaml
with:
  suite: Regression
  focus: "Consolidation"
  test_name: "e2e-with-deletion-cost"
  pod_deletion_cost_enabled: "true"
  pod_deletion_cost_ranking_strategy: "LargestToSmallest"
  pod_deletion_cost_change_detection: "true"
```

This configuration:
- Enables the feature for functional testing
- Uses a simpler strategy for predictable behavior
- Validates feature works correctly in e2e scenarios

### Example 3: Local Testing with Makefile

Test the feature locally using environment variables:

```bash
# Test with feature disabled (default)
make apply-with-kind

# Test with feature enabled using Random strategy
POD_DELETION_COST_ENABLED=true make apply-with-kind

# Test with feature enabled using UnallocatedVCPUPerPodCost strategy
POD_DELETION_COST_ENABLED=true \
POD_DELETION_COST_RANKING_STRATEGY=UnallocatedVCPUPerPodCost \
make apply-with-kind

# Test with all options configured
POD_DELETION_COST_ENABLED=true \
POD_DELETION_COST_RANKING_STRATEGY=SmallestToLargest \
POD_DELETION_COST_CHANGE_DETECTION=false \
make apply-with-kind
```

After deployment, verify the configuration:

```bash
# Check controller environment variables
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env}' | jq '.'

# Check feature gate
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FEATURE_GATES")].value}'

# Check ranking strategy
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="POD_DELETION_COST_RANKING_STRATEGY")].value}'
```

### Example 4: Matrix Testing

Run tests across multiple configurations to compare performance:

```yaml
# .github/workflows/pod-deletion-cost-matrix.yaml
name: Pod Deletion Cost Matrix Testing
on:
  workflow_dispatch:
    inputs:
      test_focus:
        description: 'Ginkgo focus pattern'
        required: false
        default: 'Basic Deployment'

jobs:
  matrix-test:
    strategy:
      fail-fast: false
      matrix:
        config:
          - name: "Baseline (Disabled)"
            enabled: "false"
            strategy: "Random"
            change_detection: "true"
          
          - name: "Random Strategy"
            enabled: "true"
            strategy: "Random"
            change_detection: "true"
          
          - name: "Unallocated vCPU"
            enabled: "true"
            strategy: "UnallocatedVCPUPerPodCost"
            change_detection: "true"
    
    uses: ./.github/workflows/e2e.yaml
    with:
      suite: Performance
      focus: ${{ inputs.test_focus }}
      test_name: ${{ matrix.config.name }}
      pod_deletion_cost_enabled: ${{ matrix.config.enabled }}
      pod_deletion_cost_ranking_strategy: ${{ matrix.config.strategy }}
      pod_deletion_cost_change_detection: ${{ matrix.config.change_detection }}
```

Trigger the matrix workflow:

```bash
# Via GitHub UI: Actions → Pod Deletion Cost Matrix Testing → Run workflow

# Or via GitHub CLI
gh workflow run pod-deletion-cost-matrix.yaml -f test_focus="Basic Deployment"
```

This will run tests in parallel with different configurations and generate separate performance reports for comparison.

### Example 5: Custom Test Configuration

Create a custom workflow for specific testing scenarios:

```yaml
name: Custom Pod Deletion Cost Test
on:
  workflow_dispatch:
    inputs:
      enabled:
        description: 'Enable feature'
        required: true
        default: 'true'
      strategy:
        description: 'Ranking strategy'
        required: true
        default: 'UnallocatedVCPUPerPodCost'

jobs:
  custom-test:
    uses: ./.github/workflows/e2e.yaml
    with:
      suite: Performance
      focus: "Wide Deployments"
      test_name: "custom-${{ inputs.strategy }}"
      pod_deletion_cost_enabled: ${{ inputs.enabled }}
      pod_deletion_cost_ranking_strategy: ${{ inputs.strategy }}
      pod_deletion_cost_change_detection: "true"
```

## Troubleshooting

### Issue: Feature Not Enabled Despite Configuration

**Symptoms**:
- Workflow shows feature enabled in inputs
- Controller environment variables show feature disabled
- No pod annotations are being set

**Diagnosis**:
```bash
# Check what Helm actually deployed
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env}' | jq '.'

# Check if FEATURE_GATES includes PodDeletionCostManagement
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FEATURE_GATES")].value}'
```

**Solutions**:
1. Verify environment variables are exported in the workflow step:
   ```yaml
   env:
     POD_DELETION_COST_ENABLED: ${{ inputs.pod_deletion_cost_enabled }}
   ```

2. Check Makefile is reading environment variables correctly:
   ```bash
   # Test locally
   POD_DELETION_COST_ENABLED=true make apply-with-kind
   ```

3. Verify Helm command includes the controller.env settings:
   ```makefile
   --set-string controller.env[1].name=FEATURE_GATES \
   --set-string controller.env[1].value="PodDeletionCostManagement=$${POD_DELETION_COST_ENABLED:-false}"
   ```

### Issue: Pods Not Getting Annotations

**Symptoms**:
- Feature gate is enabled
- Controller is running
- Pods don't have `controller.kubernetes.io/pod-deletion-cost` annotations

**Diagnosis**:
```bash
# Check controller logs for errors
kubectl logs -n kube-system deployment/karpenter --tail=100 | grep -i "deletion.*cost"

# Check if any pods have annotations
kubectl get pods -A -o json | jq '[.items[] | select(.metadata.annotations."controller.kubernetes.io/pod-deletion-cost" != null)] | length'

# List pods with annotations
kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.annotations.controller\.kubernetes\.io/pod-deletion-cost}{"\n"}{end}' | grep -v "^$"
```

**Solutions**:
1. Wait longer - annotations are set asynchronously:
   ```bash
   # Wait 60 seconds after deployment
   sleep 60
   kubectl get pods -A -o json | jq '[.items[] | select(.metadata.annotations."controller.kubernetes.io/pod-deletion-cost" != null)] | length'
   ```

2. Check if pods are eligible for annotation (must be scheduled on Karpenter-managed nodes):
   ```bash
   # Check which nodes are managed by Karpenter
   kubectl get nodes -l karpenter.sh/nodepool
   
   # Check which pods are on those nodes
   kubectl get pods -A -o wide | grep <node-name>
   ```

3. Verify the controller has permission to update pod annotations:
   ```bash
   # Check RBAC permissions
   kubectl get clusterrole karpenter -o yaml | grep -A 5 "pods"
   ```

### Issue: Wrong Ranking Strategy Applied

**Symptoms**:
- Workflow input specifies one strategy
- Controller uses a different strategy
- Performance results don't match expectations

**Diagnosis**:
```bash
# Check what strategy is actually configured
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="POD_DELETION_COST_RANKING_STRATEGY")].value}'

# Check controller logs for strategy initialization
kubectl logs -n kube-system deployment/karpenter --tail=100 | grep -i "ranking.*strategy"
```

**Solutions**:
1. Verify the environment variable is set correctly in the workflow:
   ```yaml
   env:
     POD_DELETION_COST_RANKING_STRATEGY: ${{ inputs.pod_deletion_cost_ranking_strategy }}
   ```

2. Check for typos in strategy name (case-sensitive):
   - Valid: `UnallocatedVCPUPerPodCost`
   - Invalid: `unallocatedvcpuperpodcost`, `UnallocatedVcpuPerPodCost`

3. Verify Makefile passes the value correctly:
   ```bash
   # Test locally with explicit value
   POD_DELETION_COST_RANKING_STRATEGY=UnallocatedVCPUPerPodCost make apply-with-kind
   ```

### Issue: Performance Reports Missing Configuration

**Symptoms**:
- Performance reports don't include pod deletion cost fields
- Can't correlate performance with configuration

**Diagnosis**:
```bash
# Download and check report artifact
unzip performance-report.zip
cat performance-report.json | jq '.pod_deletion_cost_enabled'
```

**Solutions**:
1. Verify test suite reads environment variables:
   ```go
   // In suite_test.go
   podDeletionCostEnabled = func() bool {
       if val := os.Getenv("POD_DELETION_COST_ENABLED"); val != "" {
           return val == "true"
       }
       return true
   }()
   ```

2. Check report initialization includes configuration:
   ```go
   // In report.go
   func NewPerformanceReport(testName string) *PerformanceReport {
       return &PerformanceReport{
           PodDeletionCostEnabled: podDeletionCostEnabled,
           // ...
       }
   }
   ```

3. Verify environment variables are exported before test execution:
   ```yaml
   - name: run e2e tests
     env:
       POD_DELETION_COST_ENABLED: ${{ inputs.pod_deletion_cost_enabled }}
       POD_DELETION_COST_RANKING_STRATEGY: ${{ inputs.pod_deletion_cost_ranking_strategy }}
   ```

### Issue: Verification Steps Failing

**Symptoms**:
- Workflow fails at verification step
- Configuration appears correct
- Tests would pass if verification was skipped

**Diagnosis**:
```bash
# Check what verification step is failing
# Look at workflow logs for the specific verification step

# Manually run verification commands
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FEATURE_GATES")].value}'
```

**Solutions**:
1. If feature is disabled, verification steps should be skipped:
   ```yaml
   - name: Verify Karpenter configuration
     if: inputs.pod_deletion_cost_enabled == 'true'
   ```

2. Adjust timing - controller might not be ready:
   ```yaml
   - name: Wait for controller
     run: |
       kubectl wait --for=condition=available --timeout=300s deployment/karpenter -n kube-system
   ```

3. Check if jq is available in the runner:
   ```yaml
   - name: Install dependencies
     run: |
       sudo apt-get update && sudo apt-get install -y jq
   ```

### Issue: Local Testing Not Matching CI

**Symptoms**:
- Tests pass locally but fail in CI
- Configuration appears identical
- Behavior differs between environments

**Diagnosis**:
```bash
# Compare local and CI configurations
echo "Local:"
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env}' | jq '.'

# Check CI logs for the same output
```

**Solutions**:
1. Ensure local environment variables match CI:
   ```bash
   # Export variables before running make
   export POD_DELETION_COST_ENABLED=true
   export POD_DELETION_COST_RANKING_STRATEGY=UnallocatedVCPUPerPodCost
   export POD_DELETION_COST_CHANGE_DETECTION=true
   make apply-with-kind
   ```

2. Check for differences in Kubernetes versions:
   ```bash
   kubectl version --short
   ```

3. Verify the same Karpenter image is used:
   ```bash
   kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].image}'
   ```

### Common Configuration Mistakes

1. **Using boolean instead of string in workflow inputs**:
   ```yaml
   # Wrong
   pod_deletion_cost_enabled: true
   
   # Correct
   pod_deletion_cost_enabled: "true"
   ```

2. **Forgetting to export environment variables**:
   ```yaml
   # Wrong - variables not available to make
   - name: install controller
     run: make apply-with-kind
   
   # Correct - variables exported
   - name: install controller
     env:
       POD_DELETION_COST_ENABLED: ${{ inputs.pod_deletion_cost_enabled }}
     run: make apply-with-kind
   ```

3. **Incorrect Makefile variable syntax**:
   ```makefile
   # Wrong - single $ doesn't work in Makefile
   --set-string controller.env[1].value="PodDeletionCostManagement=${POD_DELETION_COST_ENABLED:-false}"
   
   # Correct - double $$ for Makefile escaping
   --set-string controller.env[1].value="PodDeletionCostManagement=$${POD_DELETION_COST_ENABLED:-false}"
   ```

4. **Case sensitivity in strategy names**:
   ```bash
   # Wrong
   POD_DELETION_COST_RANKING_STRATEGY=unallocatedvcpuperpodcost
   
   # Correct
   POD_DELETION_COST_RANKING_STRATEGY=UnallocatedVCPUPerPodCost
   ```

### Debugging Tips

1. **Enable verbose logging in controller**:
   ```bash
   # Add to Helm values
   --set-string controller.env[4].name=LOG_LEVEL \
   --set-string controller.env[4].value=debug
   ```

2. **Check controller startup logs**:
   ```bash
   kubectl logs -n kube-system deployment/karpenter --tail=200 | grep -i "feature\|deletion\|cost"
   ```

3. **Monitor pod annotation updates in real-time**:
   ```bash
   watch -n 5 'kubectl get pods -A -o json | jq "[.items[] | select(.metadata.annotations.\"controller.kubernetes.io/pod-deletion-cost\" != null)] | length"'
   ```

4. **Compare configurations across test runs**:
   ```bash
   # Save configuration from each run
   kubectl get deployment -n kube-system karpenter -o yaml > karpenter-config-$(date +%s).yaml
   
   # Diff configurations
   diff karpenter-config-1.yaml karpenter-config-2.yaml
   ```

5. **Validate JSON in performance reports**:
   ```bash
   cat performance-report.json | jq '.' > /dev/null && echo "Valid JSON" || echo "Invalid JSON"
   ```

## Quick Reference

### Configuration Options

| Parameter | Type | Valid Values | Default (E2E) | Default (Perf) | Description |
|-----------|------|--------------|---------------|----------------|-------------|
| `pod_deletion_cost_enabled` | string | `"true"`, `"false"` | `"false"` | `"true"` | Enable/disable the feature |
| `pod_deletion_cost_ranking_strategy` | string | `"Random"`, `"LargestToSmallest"`, `"SmallestToLargest"`, `"UnallocatedVCPUPerPodCost"` | `"Random"` | `"UnallocatedVCPUPerPodCost"` | Pod ranking strategy |
| `pod_deletion_cost_change_detection` | string | `"true"`, `"false"` | `"true"` | `"true"` | Enable change detection optimization |

### Environment Variables

| Variable | Makefile Default | Test Suite Default | Controller Usage |
|----------|------------------|-------------------|------------------|
| `POD_DELETION_COST_ENABLED` | `"false"` | `true` (perf tests) | Sets feature gate |
| `POD_DELETION_COST_RANKING_STRATEGY` | `"Random"` | `"UnallocatedVCPUPerPodCost"` (perf tests) | Sets ranking algorithm |
| `POD_DELETION_COST_CHANGE_DETECTION` | `"true"` | `true` | Enables optimization |

### Ranking Strategies Comparison

| Strategy | Use Case | Complexity | Performance Impact | Best For |
|----------|----------|------------|-------------------|----------|
| `Random` | Baseline testing | Low | Minimal | Testing, debugging |
| `LargestToSmallest` | Consolidation efficiency | Low | Low | Reducing node count |
| `SmallestToLargest` | Minimize disruption | Low | Low | Stable workloads |
| `UnallocatedVCPUPerPodCost` | Optimal resource utilization | High | Medium | Production, performance testing |

### Workflow Configuration Matrix

| Workflow | Enabled | Strategy | Change Detection | Purpose |
|----------|---------|----------|------------------|---------|
| **E2E (default)** | `false` | `Random` | `true` | Baseline functional testing |
| **E2E (manual)** | configurable | configurable | configurable | Custom testing scenarios |
| **Performance** | `true` | `UnallocatedVCPUPerPodCost` | `true` | Performance validation |
| **Matrix** | varies | varies | varies | Strategy comparison |

### Common Commands

#### Local Testing

```bash
# Default (feature disabled)
make apply-with-kind

# Enable with Random strategy
POD_DELETION_COST_ENABLED=true make apply-with-kind

# Enable with UnallocatedVCPU strategy
POD_DELETION_COST_ENABLED=true POD_DELETION_COST_RANKING_STRATEGY=UnallocatedVCPUPerPodCost make apply-with-kind

# Full configuration
POD_DELETION_COST_ENABLED=true \
POD_DELETION_COST_RANKING_STRATEGY=SmallestToLargest \
POD_DELETION_COST_CHANGE_DETECTION=false \
make apply-with-kind
```

#### Verification

```bash
# Check feature gate
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FEATURE_GATES")].value}'

# Check strategy
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="POD_DELETION_COST_RANKING_STRATEGY")].value}'

# Check change detection
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="POD_DELETION_COST_CHANGE_DETECTION")].value}'

# Count annotated pods
kubectl get pods -A -o json | jq '[.items[] | select(.metadata.annotations."controller.kubernetes.io/pod-deletion-cost" != null)] | length'

# View sample annotations
kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.controller\.kubernetes\.io/pod-deletion-cost}{"\n"}{end}' | grep -v "^[^\t]*\t$" | head -10
```

#### Workflow Triggers

```bash
# Trigger e2e workflow with feature enabled
gh workflow run e2e.yaml \
  -f suite=Regression \
  -f focus="Consolidation" \
  -f pod_deletion_cost_enabled="true" \
  -f pod_deletion_cost_ranking_strategy="UnallocatedVCPUPerPodCost"

# Trigger performance workflow (feature enabled by default)
gh workflow run kind-perf-e2e.yaml

# Trigger matrix workflow
gh workflow run pod-deletion-cost-matrix.yaml -f test_focus="Basic Deployment"
```

### File Locations

| File | Purpose |
|------|---------|
| `.github/workflows/e2e.yaml` | Main e2e workflow with pod deletion cost inputs |
| `.github/workflows/kind-perf-e2e.yaml` | Performance workflow (feature enabled by default) |
| `.github/workflows/pod-deletion-cost-matrix.yaml` | Matrix testing workflow |
| `Makefile` | Build system with environment variable support |
| `test/suites/performance/suite_test.go` | Test suite configuration |
| `test/suites/performance/report.go` | Performance report structure |
| `.github/POD_DELETION_COST_CI_SETUP.md` | This documentation |

### Related Documentation

- [Pod Deletion Cost Feature Documentation](../pkg/controllers/pod/deletioncost/README.md)
- [Pod Deletion Cost Quick Start](../POD_DELETION_COST_QUICK_START.md)
- [Pod Deletion Cost Testing Guide](../pkg/controllers/pod/deletioncost/TESTING.md)
- [GitHub Actions Reusable Workflows](https://docs.github.com/en/actions/using-workflows/reusing-workflows)
- [Helm Values Documentation](https://helm.sh/docs/chart_template_guide/values_files/)

### Support and Feedback

If you encounter issues not covered in this guide:

1. Check the [Troubleshooting](#troubleshooting) section above
2. Review controller logs: `kubectl logs -n kube-system deployment/karpenter`
3. Check workflow run logs in GitHub Actions
4. Verify configuration with the verification commands above
5. Compare your configuration with the working examples

For questions or improvements to this documentation, please create an issue or submit a pull request.
