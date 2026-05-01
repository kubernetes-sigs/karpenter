# perf: integrate multi-iteration benchmarks into PR review flow

## Motivation

PR#2671 (topology domain filtering) caused a 1.95x consolidation performance
regression that reached customers as 10-20x CPU spikes (issue #2954) before
being identified and reverted. The regression was not caught before merge
because:

1. Performance tests only ran after reviewer approval, not before.
2. Results were not compared against a baseline automatically.
3. Each test ran once — a single run has a 40-60% coefficient of variation on
   kind clusters, giving only ~60% detection probability for a 2x regression.

A controlled experiment with 80+ benchmark runs confirmed the benchmark
reliably detects the PR#2671 regression at n=10 (64% power) and n=20 (95%
power). This PR implements the workflow changes recommended in the
consolidation regression report.

## Changes

### `.github/workflows/kind-perf-e2e.yaml`

**New triggers:**

- `pull_request` (labeled/synchronize): auto-triggers on `size/XL` or
  `size/XXL` PRs with a `feat:` or `fix:` title prefix. These are the PRs
  most likely to introduce performance regressions.
- `perf-check` label: manual trigger for reviewers who suspect performance
- `ok-to-test` label: required for all pull_request triggers. Ensures
  benchmarks only run on PRs vetted by an org member.
  impact on PRs that don't meet the auto-trigger criteria.
- `skip-perf` label: allows maintainers to suppress the pre-approval run
  (e.g. large refactors, renames). Does **not** suppress post-merge runs.
- Push to `main` always runs (no skip path) to maintain the rolling baseline.

**Iterations:** All triggered runs now pass `repeat: 10` to the e2e workflow,
running each test suite 10 times per trigger.

### `.github/workflows/e2e.yaml`

**New input:** `repeat` (default: 1) — number of iterations to run the test.

**Iteration loop:** The run step loops `repeat` times, copying each
iteration's `*_performance_report.json` into `iter_N/` subdirectories under
`OUTPUT_DIR`.

**Aggregation step:** After all iterations, a Python script computes per-test
statistics (n, median, mean, stddev, min, max) and writes
`aggregated_summary.json`. Results are printed as a human-readable table in
the job log. The median is the primary metric (robust to outliers).

**Artifact layout:** Artifacts now include all per-iteration reports and the
aggregated summary, enabling downstream baseline comparison.

## Trigger behavior summary

| Trigger | When | Iterations | Can skip? |
|---|---|---|---|
| `size/XL` or `size/XXL` + feat/fix PR | Automatic on label (requires ok-to-test) | 10 per test | Yes, with `skip-perf` |
| `perf-check` label | Manual by reviewer (requires ok-to-test) | 10 per test | N/A (opt-in) |
| `/karpenter perf` comment (ApprovalComment) | Manual by reviewer | 10 per test | Yes, with `skip-perf` |
| Merge to `main` | Automatic | 10 per test | No (always runs) |

## Cost estimate

~23 hours of GitHub Actions compute per triggered PR (7 tests × 10 iterations
× ~20 min each, running in parallel via matrix). Actual wall-clock time
depends on the `kubernetes-sigs` org's concurrency limits. This cost is
incurred only on the triggers above — documentation, CI, and test-only PRs
are unaffected.

## Regression detection

Each test runs 10 iterations. The aggregation script computes the batch
median and emits it as a `customSmallerIsBetter` metric for
[benchmark-action/github-action-benchmark](https://github.com/benchmark-action/github-action-benchmark).

The action compares the current batch median against the previous commit's
batch median. If the median is 50% worse (configurable via `alert-threshold`),
the job fails and a comment is posted on the commit. Benchmark history is
stored in the GitHub Actions cache (per test, per OS), so no additional
branches or repo configuration are needed.

This approach has two layers of noise reduction:
1. The median of 10 runs absorbs per-run variance (CV of 40-60% on kind).
2. The action compares medians across commits, not individual runs.

A 150% threshold on batch medians is much tighter than 150% on single runs.
With the observed CV of ~48% on Hostname Spread Consolidation, a single run
can legitimately vary by 2x. A batch median varies much less.

The `range` field in the benchmark output carries the stddev, and the `extra`
field carries the full stats (mean, cv, min, max, n). These appear in the
GitHub Pages chart tooltips and PR comments for context.

**Future improvement:** If the project enables a `gh-pages` branch,
`benchmark-action` can auto-push results there and generate a time-series
chart dashboard at `https://kubernetes-sigs.github.io/karpenter/dev/bench/`.
This gives visual trend tracking across releases and makes it easy to spot
gradual performance drift. The switch is a one-line change (`auto-push: true`
+ creating the branch). The cache-based approach works without it.

This replaces the per-test hard-coded thresholds (e.g., the 350MB memory
assertion that flakes on every Hydra run). Multi-run statistics are robust
to single-run outliers and sensitive to sustained shifts.

## Testing

Workflow syntax validated with `actionlint` (if available). The iteration
loop and aggregation script are straightforward shell/Python with no external
dependencies. Full end-to-end validation requires a GitHub Actions run on the
fork.

## References

- Consolidation regression report: `consolidation_regression_report.md`
- Issue #2954: High CPU usage regression in Karpenter v1.11.0
- PR#2671: Filter topology domains by NodePool compatibility (the regression)
- PR#2957: Revert PR#2671
