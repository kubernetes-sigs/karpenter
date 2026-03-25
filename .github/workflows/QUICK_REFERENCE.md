# Quick Reference: Running Decision Ratio Tests in GitHub Actions

## Option 1: Run Matrix Test (Recommended for Comparison)

**Best for:** Comparing multiple thresholds at once

1. Go to: **Actions** → **Decision Ratio Threshold Matrix**
2. Click **Run workflow**
3. (Optional) Customize test focus and name
4. Click **Run workflow** button

This runs tests with thresholds: 0.5, 1.0, 1.5, 2.0 in parallel.

## Option 2: Modify Standard Performance Test

**Best for:** Changing the default threshold for regular performance tests

Edit `.github/workflows/kind-perf-e2e.yaml`:

```yaml
decision_ratio_threshold: "1.5"  # Change to desired value
```

Commit and push. The workflow runs automatically on push to main.

## Option 3: Create Custom Workflow

**Best for:** One-off tests or specific scenarios

Create `.github/workflows/my-test.yaml`:

```yaml
name: My Custom Test
on:
  workflow_dispatch:
    inputs:
      threshold:
        description: 'Decision ratio threshold'
        default: "1.5"
jobs:
  test:
    uses: ./.github/workflows/e2e.yaml
    with:
      suite: Performance
      focus: "Basic Deployment"
      test_name: "My Test"
      decision_ratio_threshold: ${{ inputs.threshold }}
```

## Downloading Results

1. Go to the completed workflow run
2. Scroll to **Artifacts** section at bottom
3. Download `performance-results-*` artifacts
4. Extract and examine JSON files

## Quick Comparison

```bash
# After downloading artifacts
for dir in performance-results-*/; do
  echo "=== $(basename $dir) ==="
  jq '{threshold: .decision_ratio_threshold, time: .total_time, disrupted: .pods_disrupted}' \
    "$dir/consolidation_performance_report.json"
done
```

## Common Threshold Values

- **0.5** - Aggressive (consolidates more, faster)
- **1.0** - Balanced (break-even point)
- **1.5** - Conservative (default, requires 50% more savings)
- **2.0** - Very Conservative (requires 100% more savings)

## See Also

- [Detailed Workflow Documentation](./DECISION_RATIO_WORKFLOWS.md)
- [Local Testing Guide](../../test/suites/performance/QUICK_START.md)
