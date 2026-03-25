# GitHub Actions Integration Summary

## Overview

This document summarizes the integration of the Pod Deletion Cost Management feature into the Karpenter GitHub Actions CI/CD pipeline. The integration enables automated testing of the feature by making it configurable through workflow inputs and environment variables.

## What Was Done

The integration adds support for configuring the Pod Deletion Cost Management feature in CI/CD workflows while maintaining backward compatibility. The feature is:
- **Disabled by default** in regular end-to-end tests
- **Enabled by default** in performance tests with the `UnallocatedVCPUPerPodCost` strategy
- **Fully configurable** through workflow inputs and environment variables

## Modified Files

### Workflow Files
- **`.github/workflows/e2e.yaml`**
  - Added workflow inputs: `pod_deletion_cost_enabled`, `pod_deletion_cost_ranking_strategy`, `pod_deletion_cost_change_detection`
  - Added environment variable exports in the "install kwok and controller" step
  - Added verification steps to check Karpenter configuration and pod annotations

- **`.github/workflows/kind-perf-e2e.yaml`**
  - Configured to enable pod deletion cost by default
  - Set `pod_deletion_cost_ranking_strategy` to `UnallocatedVCPUPerPodCost`
  - Passes configuration to the reusable e2e workflow

- **`.github/workflows/pod-deletion-cost-matrix.yaml`** (new)
  - Created matrix testing workflow for comparing different configurations
  - Tests all ranking strategies and baseline (disabled) configuration
  - Allows manual triggering with custom test focus patterns

### Build Configuration
- **`Makefile`**
  - Modified `apply-with-kind` target to read environment variables
  - Added support for `POD_DELETION_COST_ENABLED`, `POD_DELETION_COST_RANKING_STRATEGY`, `POD_DELETION_COST_CHANGE_DETECTION`
  - Passes values to Helm as controller environment variables
  - Includes documentation comments with usage examples

### Test Suite
- **`test/suites/performance/suite_test.go`**
  - Enhanced variable initialization to read from environment variables
  - Added configuration logging at test startup
  - Added validation for ranking strategy values
  - Defaults match performance test workflow configuration

- **`test/suites/performance/report.go`**
  - Added pod deletion cost fields to `PerformanceReport` struct
  - Populates configuration fields from test suite variables
  - Includes configuration in JSON output for analysis

### Documentation
- **`.github/POD_DELETION_COST_CI_SETUP.md`** (new)
  - Comprehensive CI setup guide
  - Documents all workflow inputs and environment variables
  - Provides usage examples for different scenarios
  - Includes troubleshooting guide and quick reference table

- **`README.md`**
  - Added "Testing Pod Deletion Cost Feature" section
  - Documents CI behavior and local testing workflow
  - Links to detailed CI setup guide

- **`GITHUB_ACTIONS_INTEGRATION.md`** (this file)
  - Integration summary and verification checklist

## Current Configuration

### Default Behavior

| Workflow Type | Feature Enabled | Ranking Strategy | Change Detection |
|--------------|----------------|------------------|------------------|
| E2E Tests | No | Random | Yes |
| Performance Tests | Yes | UnallocatedVCPUPerPodCost | Yes |
| Matrix Tests | Varies | Varies | Varies |

### Environment Variables

The following environment variables control the feature configuration:

- `POD_DELETION_COST_ENABLED` - Enable/disable feature (default: "false")
- `POD_DELETION_COST_RANKING_STRATEGY` - Ranking strategy (default: "Random")
- `POD_DELETION_COST_CHANGE_DETECTION` - Enable change detection (default: "true")

### Workflow Inputs

The e2e workflow accepts the following inputs:

- `pod_deletion_cost_enabled` (string) - Enable feature (default: "false")
- `pod_deletion_cost_ranking_strategy` (string) - Strategy (default: "Random")
- `pod_deletion_cost_change_detection` (string) - Change detection (default: "true")

## Verification Checklist

Use this checklist to verify the integration is working correctly:

### Local Development
- [ ] Run `make apply-with-kind` without environment variables
  - Verify feature is disabled (check deployment env vars)
- [ ] Run with `POD_DELETION_COST_ENABLED=true make apply-with-kind`
  - Verify feature gate includes `PodDeletionCostManagement=true`
  - Verify controller has correct environment variables
- [ ] Run with all environment variables set
  - Verify Helm values are correctly applied
  - Verify pods receive deletion cost annotations

### E2E Workflow
- [ ] Trigger e2e workflow manually without inputs
  - Verify feature is disabled by default
  - Verify tests pass
- [ ] Trigger with `pod_deletion_cost_enabled: "true"`
  - Verify feature gate is enabled
  - Verify verification steps pass
  - Verify pods have annotations

### Performance Workflow
- [ ] Trigger performance workflow
  - Verify feature is enabled by default
  - Verify `UnallocatedVCPUPerPodCost` strategy is used
  - Verify verification steps pass
  - Download and check performance report includes configuration

### Matrix Workflow
- [ ] Trigger matrix workflow
  - Verify all matrix configurations run
  - Verify each configuration uses correct settings
  - Verify baseline (disabled) configuration runs
  - Compare results across different strategies

### Verification Commands

Check Karpenter deployment configuration:
```bash
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env}' | jq '.'
```

Check feature gate:
```bash
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FEATURE_GATES")].value}'
```

Check ranking strategy:
```bash
kubectl get deployment -n kube-system karpenter -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="POD_DELETION_COST_RANKING_STRATEGY")].value}'
```

Check pod annotations:
```bash
kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.controller\.kubernetes\.io/pod-deletion-cost}{"\n"}{end}' | head -10
```

## Troubleshooting

### Feature Not Enabled in CI
1. Check workflow inputs are passed correctly
2. Verify environment variables are exported in workflow
3. Check Makefile receives environment variables
4. Verify Helm values are applied correctly

### Pods Not Annotated
1. Verify feature gate is enabled
2. Check controller logs for errors
3. Ensure pods are managed by Karpenter
4. Wait 30-60 seconds for annotations to appear

### Performance Reports Missing Configuration
1. Verify test suite reads environment variables
2. Check report initialization includes configuration fields
3. Ensure JSON serialization includes new fields

### Local Testing Issues
1. Verify environment variables are exported before running make
2. Check Makefile syntax for variable expansion
3. Verify Helm chart accepts controller environment variables
4. Check controller logs for configuration errors

## Next Steps

### Recommended Actions
1. Run the verification checklist to ensure all components work correctly
2. Monitor performance test results to validate feature impact
3. Run matrix tests to compare different ranking strategies
4. Review performance reports to correlate metrics with configuration

### Future Enhancements
- Add automated performance comparison reports
- Create dashboard for tracking metrics across strategies
- Implement automated regression detection
- Add strategy recommendation based on workload patterns

## References

- [Pod Deletion Cost CI Setup Guide](.github/POD_DELETION_COST_CI_SETUP.md)
- [Pod Deletion Cost Design Document](designs/pod-deletion-cost-management.md)
- [GitHub Actions Reusable Workflows](https://docs.github.com/en/actions/using-workflows/reusing-workflows)
- [Karpenter Feature Gates](https://karpenter.sh/docs/concepts/settings/#feature-gates)
