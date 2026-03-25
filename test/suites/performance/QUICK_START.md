# Quick Start: Testing Decision Ratio Thresholds

## Run All Thresholds Automatically

```bash
cd test/suites/performance
./run_decision_ratio_tests.sh
```

This will test with thresholds: 0.5, 1.0, 1.5, and 2.0

## Run Single Test with Custom Threshold

```bash
cd test/suites/performance

# Aggressive (more consolidation)
DECISION_RATIO_THRESHOLD=0.5 OUTPUT_DIR=./results_0.5 go test -v -timeout 60m -run "Performance" ./basic_test.go

# Balanced
DECISION_RATIO_THRESHOLD=1.0 OUTPUT_DIR=./results_1.0 go test -v -timeout 60m -run "Performance" ./basic_test.go

# Conservative (default)
DECISION_RATIO_THRESHOLD=1.5 OUTPUT_DIR=./results_1.5 go test -v -timeout 60m -run "Performance" ./basic_test.go

# Very Conservative (less consolidation)
DECISION_RATIO_THRESHOLD=2.0 OUTPUT_DIR=./results_2.0 go test -v -timeout 60m -run "Performance" ./basic_test.go
```

## View Results

```bash
# Check console output
cat results_1.5/test_output.log

# View JSON report (if jq is installed)
jq '.' results_1.5/consolidation_performance_report.json

# Compare key metrics across thresholds
for dir in results_*; do
  echo "=== $dir ==="
  jq '{threshold: .decision_ratio_threshold, time: .total_time, disrupted: .pods_disrupted, rounds: .rounds}' \
    "$dir/consolidation_performance_report.json"
done
```

## What to Look For

When analyzing results, compare:

1. **Total Time**: How long did consolidation take?
2. **Pods Disrupted**: How many pods were moved?
3. **Rounds**: How many consolidation rounds occurred?
4. **Nodes Removed**: How many nodes were eliminated?

Lower thresholds (0.5) should consolidate more aggressively (faster, more disruptions).
Higher thresholds (2.0) should be more conservative (slower, fewer disruptions).

## Current Test Configuration

The tests are configured with:
- **Consolidate When**: `WhenCostJustifiesDisruption`
- **Default Threshold**: `1.5`
- **Consolidate After**: `30s`
- **Budget**: `100%` (no rate limiting)

See [DECISION_RATIO_TESTING.md](./DECISION_RATIO_TESTING.md) for detailed documentation.
