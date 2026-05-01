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
| `size/XL` or `size/XXL` + feat/fix PR | Automatic on label | 10 per test | Yes, with `skip-perf` |
| `perf-check` label | Manual by reviewer | 10 per test | N/A (opt-in) |
| `/karpenter perf` comment (ApprovalComment) | Manual by reviewer | 10 per test | Yes, with `skip-perf` |
| Merge to `main` | Automatic | 10 per test | No (always runs) |

## Cost estimate

~23 hours of GitHub Actions compute per triggered PR (7 tests × 10 iterations
× ~20 min each, running in parallel via matrix). Actual wall-clock time
depends on the `kubernetes-sigs` org's concurrency limits. This cost is
incurred only on the triggers above — documentation, CI, and test-only PRs
are unaffected.

## What this does NOT include

- Baseline storage and regression flagging (comparing PR median against
  rolling baseline median ± 2σ). That requires a persistent store for
  post-merge run results and is a follow-on task once baseline data
  accumulates from the 10-iteration post-merge runs.
- Changes to the hard-coded memory/time thresholds in test files (separate
  issue).

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
