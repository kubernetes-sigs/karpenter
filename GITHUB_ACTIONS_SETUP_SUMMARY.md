# GitHub Actions Setup Summary

## What Was Changed

Updated GitHub Actions workflows to support configurable decision ratio thresholds for performance testing.

## Modified Files

### 1. `.github/workflows/e2e.yaml`
- Added `decision_ratio_threshold` input parameter (default: "1.5")
- Added `DECISION_RATIO_THRESHOLD` environment variable to test execution step

### 2. `.github/workflows/kind-perf-e2e.yaml`
- Added `decision_ratio_threshold: "1.5"` to workflow parameters

## New Files

### 1. `.github/workflows/decision-ratio-matrix.yaml`
New workflow for testing multiple thresholds in parallel:
- Tests with thresholds: 0.5, 1.0, 1.5, 2.0
- Manually triggered via workflow_dispatch
- Produces separate artifacts for each threshold

### 2. `.github/workflows/DECISION_RATIO_WORKFLOWS.md`
Comprehensive documentation covering:
- How to use each workflow
- How to create custom workflows
- How to analyze results
- Troubleshooting guide

### 3. `.github/workflows/QUICK_REFERENCE.md`
Quick reference guide with common commands and patterns

## How to Use

### Quick Start

**Run matrix test with all thresholds:**
1. Go to GitHub Actions
2. Select "Decision Ratio Threshold Matrix"
3. Click "Run workflow"

**Change default threshold:**
Edit `.github/workflows/kind-perf-e2e.yaml`:
```yaml
decision_ratio_threshold: "2.0"  # Change this
```

### Example Workflows

**Test single threshold:**
```yaml
uses: ./.github/workflows/e2e.yaml
with:
  suite: Performance
  focus: "Basic Deployment"
  decision_ratio_threshold: "1.5"
```

**Test multiple thresholds:**
```yaml
strategy:
  matrix:
    threshold: ["0.5", "1.0", "1.5", "2.0"]
uses: ./.github/workflows/e2e.yaml
with:
  suite: Performance
  decision_ratio_threshold: ${{ matrix.threshold }}
```

## Integration with Test Suite

The workflow parameter flows through to the test suite:

1. **Workflow Input** → `decision_ratio_threshold: "1.5"`
2. **Environment Variable** → `DECISION_RATIO_THRESHOLD=1.5`
3. **Test Suite** → Reads env var in `suite_test.go`
4. **NodePool Config** → Sets `DecisionRatioThreshold` field
5. **Performance Report** → Captures threshold in JSON output

## Results

Each test run produces artifacts containing:
- JSON performance reports with threshold value
- Console output logs
- Metrics for comparison

Example report structure:
```json
{
  "consolidate_when": "WhenCostJustifiesDisruption",
  "decision_ratio_threshold": 1.5,
  "total_time": "5m30s",
  "pods_disrupted": 150,
  "rounds": 3
}
```

## Next Steps

1. Review [Quick Reference](.github/workflows/QUICK_REFERENCE.md) for common usage
2. Read [Detailed Documentation](.github/workflows/DECISION_RATIO_WORKFLOWS.md) for advanced scenarios
3. Run the matrix workflow to compare thresholds
4. Analyze results and document findings

## Backward Compatibility

All changes are backward compatible:
- Default threshold is 1.5 (conservative)
- Existing workflows continue to work
- New parameter is optional with sensible default
