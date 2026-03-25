# Decision Ratio Threshold Testing Guide

This guide explains how to test the `WhenCostJustifiesDisruption` consolidation policy with different decision ratio thresholds.

## Overview

The decision ratio threshold controls how conservative Karpenter is when deciding whether to consolidate nodes. The decision ratio compares normalized cost savings to normalized disruption cost:

- **Lower thresholds (e.g., 0.5)**: More aggressive consolidation - will consolidate even with smaller cost savings
- **Higher thresholds (e.g., 2.0)**: More conservative consolidation - requires larger cost savings to justify disruption

## Configuration

The performance test suite now captures and reports:
- `ConsolidateWhen` policy (e.g., "WhenCostJustifiesDisruption")
- `DecisionRatioThreshold` value
- All standard performance metrics (time, disruptions, rounds, etc.)

### Default Settings

By default, the performance tests use:
- **ConsolidateWhen**: `WhenCostJustifiesDisruption`
- **DecisionRatioThreshold**: `1.5` (conservative)

### Customizing the Threshold

You can override the decision ratio threshold using the `DECISION_RATIO_THRESHOLD` environment variable:

```bash
# Run with aggressive threshold (0.5)
DECISION_RATIO_THRESHOLD=0.5 go test -v -timeout 60m -run "Performance" ./basic_test.go

# Run with balanced threshold (1.0)
DECISION_RATIO_THRESHOLD=1.0 go test -v -timeout 60m -run "Performance" ./basic_test.go

# Run with conservative threshold (1.5) - this is the default
DECISION_RATIO_THRESHOLD=1.5 go test -v -timeout 60m -run "Performance" ./basic_test.go

# Run with very conservative threshold (2.0)
DECISION_RATIO_THRESHOLD=2.0 go test -v -timeout 60m -run "Performance" ./basic_test.go
```

## Automated Testing Script

Use the provided script to automatically run tests with multiple thresholds:

```bash
# Run with default settings (tests basic_test.go with multiple thresholds)
./run_decision_ratio_tests.sh

# Test a specific test file
TEST_PATTERN="host_name_spreading_test.go" ./run_decision_ratio_tests.sh

# Specify custom output directory
OUTPUT_DIR="./my_results" ./run_decision_ratio_tests.sh
```

The script will:
1. Run tests with thresholds: 0.5, 1.0, 1.5, 2.0
2. Save results to separate directories for each threshold
3. Generate JSON reports for analysis
4. Provide a summary of key metrics

## Output and Reports

### Console Output

During test execution, you'll see configuration information:

```
=== TEST CONFIGURATION ===
Consolidate When: WhenCostJustifiesDisruption
Decision Ratio Threshold: 1.50
==========================
```

### Performance Reports

Each test generates a JSON report with the decision ratio settings:

```json
{
  "test_name": "Consolidation Test",
  "test_type": "consolidation",
  "consolidate_when": "WhenCostJustifiesDisruption",
  "decision_ratio_threshold": 1.5,
  "total_time": "5m30s",
  "pods_disrupted": 150,
  "rounds": 3,
  ...
}
```

### Report Fields

Key fields for analysis:
- `consolidate_when`: The consolidation policy used
- `decision_ratio_threshold`: The threshold value for this test run
- `total_time`: Time taken for consolidation
- `pods_disrupted`: Number of pods disrupted during consolidation
- `rounds`: Number of consolidation rounds
- `change_in_node_count`: Net change in node count

## Analyzing Results

### Comparing Thresholds

To compare different thresholds, examine:

1. **Consolidation Time**: Does a lower threshold consolidate faster?
2. **Disruption Count**: How many pods were disrupted at each threshold?
3. **Consolidation Rounds**: How many rounds did it take?
4. **Node Efficiency**: Final node count and resource utilization

### Example Analysis

```bash
# If jq is installed, extract key metrics
for dir in performance_results/threshold_*; do
  echo "=== $(basename $dir) ==="
  jq '{
    threshold: .decision_ratio_threshold,
    time: .total_time,
    disrupted: .pods_disrupted,
    rounds: .rounds,
    nodes_removed: .change_in_node_count
  }' "$dir/consolidation_performance_report.json"
done
```

## Manual Testing

To manually test with a specific threshold:

1. Set the environment variable:
   ```bash
   export DECISION_RATIO_THRESHOLD=1.5
   ```

2. Run the test:
   ```bash
   cd test/suites/performance
   go test -v -timeout 60m -run "Performance" ./basic_test.go
   ```

3. Check the output directory for reports:
   ```bash
   ls -la $OUTPUT_DIR/*.json
   ```

## Modifying Test Configuration

To change the default threshold in code, edit `suite_test.go`:

```go
// decisionRatioThreshold controls the minimum decision ratio for consolidation
// when using WhenCostJustifiesDisruption policy. Default is 1.5 (conservative)
var decisionRatioThreshold float64 = 1.5  // Change this value
```

To change the consolidation policy, edit the `BeforeEach` block in `suite_test.go`:

```go
// Configure consolidation policy and decision ratio threshold
nodePool.Spec.Disruption.ConsolidateWhen = v1.ConsolidateWhenPolicyWhenCostJustifiesDisruption
nodePool.Spec.Disruption.DecisionRatioThreshold = &decisionRatioThreshold
```

## Troubleshooting

### Invalid Threshold Values

The decision ratio threshold must be positive (> 0). Invalid values will be rejected:

```
WARNING: Invalid DECISION_RATIO_THRESHOLD value '-1.0', using default 1.50
```

### Missing Reports

If JSON reports aren't generated, ensure the `OUTPUT_DIR` environment variable is set:

```bash
export OUTPUT_DIR="./performance_results"
go test -v -timeout 60m -run "Performance" ./basic_test.go
```

### Test Timeouts

For very conservative thresholds, consolidation may take longer. Increase the timeout:

```bash
go test -v -timeout 90m -run "Performance" ./basic_test.go
```

## Best Practices

1. **Baseline First**: Run with the default threshold (1.5) to establish a baseline
2. **Incremental Changes**: Test thresholds in small increments (0.5, 1.0, 1.5, 2.0)
3. **Multiple Runs**: Run each threshold multiple times to account for variability
4. **Document Results**: Save all JSON reports for later analysis
5. **Compare Metrics**: Focus on time, disruptions, and efficiency when comparing

## Related Documentation

- [Configurable Decision Ratio Threshold Design](../../../designs/configurable-decision-ratio-threshold-api-reference.md)
- [NodePool API Reference](../../../pkg/apis/v1/nodepool.go)
- [Performance Test Suite](./README.md)
