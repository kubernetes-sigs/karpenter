# Decision Ratio Threshold GitHub Actions Workflows

This document explains how to run performance tests with different decision ratio thresholds in GitHub Actions.

## Overview

The workflows have been updated to support configurable decision ratio thresholds for testing the `WhenCostJustifiesDisruption` consolidation policy.

## Available Workflows

### 1. Standard Performance Tests (`kind-perf-e2e.yaml`)

The standard performance workflow now includes decision ratio threshold configuration.

**Default Settings:**
- Decision Ratio Threshold: `1.5` (conservative)
- Consolidate When: `WhenCostJustifiesDisruption`

**To modify the threshold:**

Edit `.github/workflows/kind-perf-e2e.yaml`:

```yaml
uses: ./.github/workflows/e2e.yaml
with:
  suite: Performance
  focus: ${{ matrix.test.focus }}
  test_name: ${{ matrix.test.name }}
  pod_deletion_cost_enabled: "true"
  pod_deletion_cost_ranking_strategy: SmallestToLargest
  pod_deletion_cost_change_detection: "true"
  decision_ratio_threshold: "1.5"  # Change this value
```

### 2. Decision Ratio Matrix Tests (`decision-ratio-matrix.yaml`)

A dedicated workflow for testing multiple decision ratio thresholds in parallel.

**How to run:**

1. Go to GitHub Actions tab
2. Select "Decision Ratio Threshold Matrix" workflow
3. Click "Run workflow"
4. Optionally customize:
   - Test focus pattern (default: "Basic Deployment")
   - Test name for identification

**What it does:**

Runs the same test with 4 different thresholds in parallel:
- `0.5` - Aggressive consolidation
- `1.0` - Balanced consolidation
- `1.5` - Conservative consolidation (default)
- `2.0` - Very conservative consolidation

**Results:**

Each threshold run produces a separate artifact with performance reports that can be downloaded and compared.

## Workflow Parameters

### e2e.yaml Input Parameters

The base e2e workflow now accepts:

```yaml
decision_ratio_threshold:
  type: string
  required: false
  default: "1.5"
  description: "Decision ratio threshold for WhenCostJustifiesDisruption consolidation policy"
```

### How Parameters are Passed

The workflow sets the environment variable before running tests:

```yaml
- name: run the test
  env:
    DECISION_RATIO_THRESHOLD: ${{ inputs.decision_ratio_threshold }}
  run: |
    TEST_SUITE="$SUITE" make e2etests
```

The test suite reads this environment variable in `suite_test.go`:

```go
if val := os.Getenv("DECISION_RATIO_THRESHOLD"); val != "" {
    if threshold, err := strconv.ParseFloat(val, 64); err == nil && threshold > 0 {
        decisionRatioThreshold = threshold
    }
}
```

## Creating Custom Workflows

### Example: Test with Specific Threshold

Create a new workflow file (e.g., `.github/workflows/custom-threshold-test.yaml`):

```yaml
name: Custom Threshold Test
on:
  workflow_dispatch:
    inputs:
      threshold:
        description: 'Decision ratio threshold'
        required: true
        default: "1.5"
jobs:
  custom-test:
    uses: ./.github/workflows/e2e.yaml
    with:
      suite: Performance
      focus: "Basic Deployment"
      test_name: "Custom Threshold Test"
      decision_ratio_threshold: ${{ inputs.threshold }}
```

### Example: Compare Two Thresholds

```yaml
name: Threshold Comparison
on:
  workflow_dispatch:
jobs:
  comparison:
    strategy:
      matrix:
        threshold: ["0.5", "2.0"]
    uses: ./.github/workflows/e2e.yaml
    with:
      suite: Performance
      focus: "Basic Deployment"
      test_name: "Threshold ${{ matrix.threshold }}"
      decision_ratio_threshold: ${{ matrix.threshold }}
```

## Analyzing Results

### Downloading Artifacts

1. Go to the workflow run in GitHub Actions
2. Scroll to "Artifacts" section
3. Download performance results for each threshold
4. Each artifact contains JSON reports with the threshold value

### Comparing Results

The JSON reports include:

```json
{
  "consolidate_when": "WhenCostJustifiesDisruption",
  "decision_ratio_threshold": 1.5,
  "total_time": "5m30s",
  "pods_disrupted": 150,
  "rounds": 3,
  ...
}
```

Use these fields to compare:
- `decision_ratio_threshold` - The threshold used
- `total_time` - Consolidation duration
- `pods_disrupted` - Number of disrupted pods
- `rounds` - Number of consolidation rounds
- `change_in_node_count` - Nodes removed

### Example Analysis Script

```bash
#!/bin/bash
# Download and compare results from multiple threshold runs

for threshold in 0.5 1.0 1.5 2.0; do
  echo "=== Threshold: $threshold ==="
  jq '{
    threshold: .decision_ratio_threshold,
    time: .total_time,
    disrupted: .pods_disrupted,
    rounds: .rounds,
    nodes_removed: .change_in_node_count
  }' "threshold_${threshold}/consolidation_performance_report.json"
  echo ""
done
```

## Best Practices

1. **Use Matrix for Comparison**: When comparing thresholds, use the matrix workflow to run them in parallel

2. **Consistent Test Focus**: Use the same test focus pattern when comparing thresholds

3. **Multiple Runs**: Run each threshold multiple times to account for variability

4. **Document Results**: Save artifacts and document findings in issues or PRs

5. **Default Threshold**: Keep the default at 1.5 for standard performance tests

## Troubleshooting

### Threshold Not Applied

Check the test output for:
```
=== Decision Ratio Threshold set to X.XX from environment ===
```

If not present, verify:
1. The `DECISION_RATIO_THRESHOLD` env var is set in the workflow
2. The workflow is using the updated e2e.yaml

### Invalid Threshold Values

Thresholds must be positive numbers (> 0). Invalid values will fall back to default (1.5):
```
WARNING: Invalid DECISION_RATIO_THRESHOLD value 'X', using default 1.50
```

### Missing Reports

Ensure `OUTPUT_DIR` is set in the workflow. The e2e.yaml workflow handles this automatically.

## Related Documentation

- [Performance Test Suite Documentation](../../test/suites/performance/DECISION_RATIO_TESTING.md)
- [Quick Start Guide](../../test/suites/performance/QUICK_START.md)
- [Design Document](../../designs/configurable-decision-ratio-threshold-api-reference.md)
