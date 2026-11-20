# Induced regressions

Each patch undoes a known Karpenter perf fix, either as a clean revert (`rvNNNN`) or as a one-line hand mutation (`mNNNN`), against upstream main. Use them to prove a perf test catches something:

```sh
git apply test/suites/performancev2/perf/regressions/m1953.patch   # then build, deploy, run the scenario
```

## What catches what

The columns are measured layers:

- **Microbench:** `hack/bench/compare.sh` on a shared host. 10 interleaved runs of base and head, flagged at ≥ 20% and p ≤ 0.01.
- **kind e2e:** the v1-style suite (Basic, Host Name Spreading, Drift) on kind. Bounds were calibrated as 1.1 × the max of 5 local baselines.
- **EKS v2 (default sizes):** `pods` and `nodepools` at default sizes. One run per patch against 5 baselines; the bound is 1.1 × the baseline max.
- **EKS v2 (10k NodePools × 5k pods):** the `nodepools` scenario only. One run per patch against 3 baselines.
- **kind smoke (`kwok-ci`):** the per-PR layer, on kind capped at 4 CPU / 16Gi, so runner-sized. 5 to 7 baselines and one run per patch, with repeats where a first run looked like a catch. "fails" means a Stable gate breached; "warn" means an Experimental one did. Consolidation warnings are left out because lane timing confounded them: baselines ran first, while host load grew.

In every cell, "-" means not caught and "n/a" means not run.

| Patch | Undoes | Microbench | kind e2e | EKS v2 (default sizes) | EKS v2 (10k NP × 5k pods) | kind smoke (`kwok-ci`) |
|---|---|---|---|---|---|---|
| `m1953` | `Intersects` uses `HasIntersection` instead of building a set | **caught**: Scheduling1000 +46% sec/op, +521% B/op | - | - | - (CPU +3%) | - |
| `rv1973` | `Compatible` skips the `Difference()` set allocation | **caught**: +126% B/op | **caught**: drift/initial heap +22%, CPU +20% | flaky: pods/scaleOut heap +14% in one run, -1% in the calibration run; pods/consolidation time +27% | **caught**: nodepools/startup +25% | warn: nodepools CPU +49%, 2 of 3 runs |
| `rv3211` | `Requirement(s).String()` cached, fewer allocations | **caught**: nodeoverlay allocs +464-591% | - | **caught**: pods/scaleOut heap +13% and +24% (two runs) | n/a | warn: nodepools CPU +27%, once |
| `m2990` | `applyPriceOverlays` skipped on empty updates | **caught**: StoreApplyCapacityOnly +194% | n/a | n/a | n/a | - |
| `mconc1` | parallel controllers (#2252, #2270, #2293, #2324, #2406) | - | **caught**: scale-out +443%, drift +61% | **caught**: pods/scaleOut pod_ready p95 +2079%, nodepools/scheduling time +283% | n/a | **fails**: topology p95 30 s to 290 s, nodepools scheduling 7 s to 19 s |
| `m959` | anti-affinity tracks empty domains instead of scanning all | - | **caught**: drift time +33%, CPU +20% | n/a | n/a | warn: nodepools heap +35%, once |
| `m1820` | TSC and affinity iterate node domains when smaller | - | **caught**: drift/initial CPU +15% (thin) | n/a | n/a | - |
| `m1822` | `TopologyGroup.selects` parses the selector once | - | **caught**: drift/initial CPU +21% | n/a | n/a | - |
| `m1966` | `Cluster.Synced` fast path after the first sync | - | **caught**: drift +26%, CPU +20% | - | n/a | warn: topology heap +25% and nodes +31%, once |
| `m1824` | instance types pre-filtered per NodePool once | - | n/a | - | - (CPU +3.5%) | - |
| `m1945` | `InstanceType.Allocatable()` without a deep copy | - | - | - | - | - |
| `rv3310` | `intersectKeys` without per-call sets | +13% at n=6, isolated host only | - | n/a | n/a | warn: nodepools CPU +32%, once |
| `rv2232` | no deep copy in watch handlers | - | - | n/a | n/a | - |
| `rv2233` | `NewCandidate` deep copies once | - | - | n/a | n/a | warn: nodepools CPU +51%, once |
| `rv2260` | `Synced` without a deep copy | - | - | n/a | n/a | - |
| `rv2264` | node health quick checks first | - | - | n/a | n/a | - |
| `rv2013` | `Compatible` returns the first error (control) | - | - | - | **false positive**: scheduling heap +11% | - (topology heap 3 runs within baseline) |
| `rv2008` | label typo hint without edit distance (control) | - | n/a | n/a | n/a | - |

## What this says

1. **The layers are complementary.**
   - Microbenchmarks catch allocation regressions in hot functions.
   - e2e catches concurrency, topology and state-sync regressions.
   - Only `rv1973` is caught everywhere.
   - The per-PR smoke layer fails only on gross regressions (`mconc1`). Its Experimental gates warn on allocation and CPU regressions (`rv1973`, `m959`, `m1966`), which need repeats to confirm.
2. **Scheduler-internal regressions don't show end to end, even at scale.** `m1953` costs +46% in `Solve`, but only about +3% CPU at 10k NodePools × 5k pods. Gate them with Go benchmarks.
3. **Heap P95 under heavy load is GC-noisy.** At 10k × 5k, the `rv2013` control breached a heap bound set at 1.1 × the baseline max. Keep heap Stable only where baseline CV is low (`pods/scaleOut`: CV 1%).
4. **Nothing catches watch and deep-copy fixes** (`rv2232`, `rv2233`, `rv2260`, `rv2264`) or `m1945`. They need reconcile and API-call counters, or a dedicated microbenchmark.
5. **Single runs mislead.** `rv1973`'s heap catch didn't repeat, smoke startup "catches" of `m1820`, `m1953` and `rv1973` (8 to 11 s against 3 s) didn't either, and a baseline flaked on a Stable p99 bound that had been set from 3 baselines. The evidence columns in `go run ./test/suites/performancev2/cmd/inventory` show n, CV, MDE and false positive rate for every gate, so thin evidence is visible in review.
