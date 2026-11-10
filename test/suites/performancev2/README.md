# Performance suite v2

A self-contained, plugin-style perf suite. Shared scenarios live in `perf/scenarios`, the library in `perf`, and copied dependencies in `env`. This directory's `suite_test.go`, `kwok_provider.go` and `kwok_ci_provider.go` are the core KWOK provider's plugins. Other KWOK-based providers (EKS Auto) write the same two files against `perf`, and don't copy any tests.

## Three layers

| Layer | Workflow | When | Provider | What fails |
|---|---|---|---|---|
| Smoke | `perf-smoke.yaml` | every PR | `kwok-ci`: small sizes on a GitHub kind runner | Stable gates, set only to catch gross regressions |
| Periodic | `perf-periodic.yaml` | daily, 3 runs per scenario | `kwok-ci` | the same gates. Reports are kept 90 days, for trends and for the evidence to promote a gate |
| Release | `perf-release.yaml` | dispatched before a release | `kwok-ci` sweep | nothing: the sweep is measured, and `perf.html` is uploaded |

`kwok` is the reference environment: dedicated hardware and default sizes. Its levels and the large sweep below come from an EKS cluster. Runner noise means `kwok` bounds don't transfer to `kwok-ci`, so each provider has its own calibration file.

To show the smoke test catches a regression, dispatch `perf-smoke` with `regression: mconc1`. Any patch in `perf/regressions/` works.

## Audit it: `go run ./test/suites/performancev2/cmd/inventory`

The inventory prints every phase's gated and measured metrics, grouped by kind (duration, outcome, resource). Each bound comes with the calibration evidence behind it. It's generated from the same values the suite runs, plus the checked-in `calibration/<provider>.json`, and it needs no cluster.

The command fails in two cases:
- A level names an undeclared phase.
- A Stable gate lacks evidence: it needs at least 3 baseline runs, a CV of at most 20%, an estimated false positive rate of at most 1%, and at least one planted regression that breaches it.

```
| phase | kind | metric | level | bound | n | median | CV | MDE50 | MDE80 | FPR | catches |
|---|---|---|---|---|---|---|---|---|---|---|---|
| pods/scaleOut | duration | total_time_seconds | measured | - | 5 | 17 | 5% |  |  |  |  |
| pods/scaleOut | duration | pod_ready_seconds_p50 | measured | - | 5 | 9 | 0% |  |  |  |  |
| pods/scaleOut | duration | pod_ready_seconds_p95 | stable | < 18 s | 5 | 14 | 3% | 29% | 32% | <0.01% | mconc1 (+2157%) |
| pods/scaleOut | duration | pod_ready_seconds_p99 | stable | < 18 s | 5 | 14 | 3% | 29% | 32% | <0.01% | mconc1 (+2329%) |
| pods/scaleOut | duration | pod_ready_seconds_max | experimental | < 21 s | 5 | 16 | 7% | 31% | 39% | <0.01% | mconc1 (+2038%) |
| pods/scaleOut | outcome | cpu_util | experimental | > 0.741 fraction | 5 | 0.871 | 1% | 15% | 16% | <0.01% |  |
| pods/scaleOut | outcome | memory_util | experimental | > 0.689 fraction | 5 | 0.813 | 2% | 15% | 16% | <0.01% |  |
| pods/scaleOut | outcome | nodes | experimental | < 200 nodes | 5 | 162 | 2% | 23% | 25% | <0.01% |  |
| pods/scaleOut | resource | heap_mb | experimental | < 102.2 MB | 5 | 81.271 | 3% | 26% | 29% | <0.01% |  |
| pods/scaleOut | resource | cpu_cores | experimental | < 0.785 cores | 5 | 0.61 | 4% | 29% | 33% | <0.01% |  |
| nodepools/scheduling | duration | total_time_seconds | stable | < 5 s | 5 | 4 | 10% | 25% | 37% | 0.86% | mconc1 (+250%) |
...

Stable needs n >= 3, CV <= 20%, FPR <= 1% and at least one catch. An unlisted phase of an enabled scenario is measured. MDE50 and MDE80 are the smallest regressions one run catches half and 80% of the time, and FPR is the chance a baseline run breaches, assuming a normal spread with the baseline CV; with few baselines these are estimates.
```

To recalibrate, put baselines in `base-*/` and regressions in `<patch>/` directories, then run:

```sh
go run ./test/suites/performancev2/cmd/calibrate -dir runs/ -source "cluster, controller size" > /tmp/kwok.json \
  && mv /tmp/kwok.json test/suites/performancev2/calibration/kwok.json
go run ./test/suites/performancev2/cmd/calibrate -dir runs/ -provider kwok-ci -source "GitHub kind runner" > /tmp/kwok-ci.json \
  && mv /tmp/kwok-ci.json test/suites/performancev2/calibration/kwok-ci.json
```

Write to a temp file first: the calibration is embedded, and redirecting straight into it empties it before `go run` compiles.

How to read the evidence columns:
- **MDE50 and MDE80:** the smallest regression a single run breaches the bound with 50% and 80% power. MDE50 is the median's distance from the bound.
- **FPR:** the chance a baseline run breaches the bound.
- **catches:** the planted regressions that breach the bound, with their change from the baseline median.

All three model a run as normal around the baseline median with the baseline CV, so with 5 baselines they are estimates. The inventory ends with a repeats table: the runs per side a base-vs-head comparison needs to catch a given regression. Change the target with `go run ./test/suites/performancev2/cmd/inventory -mde 0.05 -fpr 0.01 -power 0.8`.

## Concepts

- **Vector:** a size knob, for example `pods` or `nodepools`.
- **Scenario:** a workload shape. It declares a default size per vector, can declare several vectors, and can declare a sweep grid over them.
- **Phase:** one measured window of a scenario, for example `pods/scaleOut`.
- **Levels:** set per provider, per phase and metric.
  - `Stable`: a breach fails the spec, after the scenario's remaining phases have run and reported.
  - `Experimental`: a breach is reported and warned about, but never fails.
  - Unlisted phase of an enabled scenario: measured, never gated. A scenario is enabled when the provider lists any of its phases.
  - `perf.Off`: the phase and the scenario's later phases are skipped, since they start from its end state.
- **Metric:** carries its own `Measures` and `OnBreach` text, so alarms are generated rather than hand-written. Every metric is in seconds, MB or cores.
- **Pod latency quantiles:** `perf.PodReady.P50/P95/P99/Max` are separate metrics. Each one measures pods' creation-to-Ready seconds over pods created during the phase, and each gets its own level.
  - Typically you gate p95 and warn on p99 and max: max is the slowest customer's experience, but it's noisier.
  - `Validate` rejects bounds that decrease across quantiles.
  - It also rejects a phase that gates both `total_time_seconds` and `pod_ready_seconds_max`, since they measure nearly the same thing.

| Scenario | Vectors (default) | Phases | What it stresses |
|---|---|---|---|
| `pods` | pods=1000 | `scaleOut`, `consolidation` | provisioning throughput and disruption cost |
| `nodepools` | nodepools=1000, pods=100 | `scheduling`, `startup` | per-NodePool cost: N-1 NodePools carry a tenant taint the pods don't tolerate |
| `topology` | pods=1000 | `scaleOut` | hard constraints: hostname anti-affinity, zonal and hostname topology spread |

## Writing a scenario

```go
var Topology = perf.Scenario{
	Name:    "topology",
	Summary: "N pods with hostname anti-affinity and zonal/hostname topology spread.",
	Vectors: map[perf.Vector]int{perf.VectorPods: 1000},
	Phases:  []perf.PhaseSpec{{Name: "scaleOut", Summary: "Topology groups and domain counting."}},
	Run: func(r *perf.Run) {
		n := r.Size(perf.VectorPods)
		anti := r.Deployment("anti", n/10, "900m", "3100Mi", test.WithPodAntiAffinityHostname())
		...
		p := r.StartPhase("scaleOut")
		r.Create(r.NodeClass, r.NodePool, anti, zone, host)
		p.AwaitScaleOut(n, 20*time.Minute)
	},
}
```

How it works:
- `Run` labels and cleans up everything it creates. A spec fails up front if an earlier one left labeled objects behind.
- `StartPhase` fails on an undeclared phase, and on a phase the provider hasn't set levels for. An empty levels entry means measure only.
- The phase scrapes the leader's `/metrics` every 2 s through the API server proxy, and fails if the controller restarts mid-phase.

## Levels (per provider)

```go
Levels: perf.Levels{
	"pods/scaleOut":     {perf.HeapMB.Stable(150), perf.CPUCores.Experimental(0.7)},
	"nodepools/startup": {perf.StartupSeconds.Stable(60)},
},
```

A provider that starts from another provider's levels declares only the differences:

```go
Levels: core.Levels.With(perf.Levels{
	"pods/scaleOut":      {perf.HeapMB.Experimental(120), perf.CPUCores.Off()}, // replace one gate, drop another
	"pods/consolidation": perf.Off,
}),
```

Promote a gate to Stable when all of these hold:
1. 20 or more main runs.
2. CV of at most 20%.
3. A bound of at least 1.1x the observed max.
4. A patch in `perf/regressions/` breaches it.

Gate wall-clock consolidation time only as Experimental.

## Scaling two vectors together: sweeps and `perf-plot`

A scenario's `Sweep` declares a grid. `KARPENTER_PERF_SWEEP=nodepools` runs one spec per cell. Sweep cells are measured, never gated, and each report records its `sizes` and the controller `commit`.

```sh
OUTPUT_DIR=/tmp/sweep KARPENTER_PERF_SWEEP=nodepools TEST_SUITE=Performancev2 make e2etests
KARPENTER_PERF_SWEEP_GRID='{"pods":[100,500],"nodepools":[1,100]}' ...   # smaller grid
go run ./test/suites/performancev2/cmd/plot -dir /tmp/sweep    # writes perf.html
```

![30-cell sweep on EKS](docs/sweep-eks.png)

This is the 30-cell `nodepools` sweep on EKS. Setup:
- **Cluster:** an EKS cluster with a tier-xl provisioned control plane.
- **Controller:** 14 CPU / 56 Gi on a dedicated m7i.4xlarge.
- **Grid:** pods [100…20000] × nodepools [1…10000], all 30 cells passing.
- **Commit:** upstream main 6eac93ba.

What it shows:
- **Scale-out time is bound by node registration.** It's about 180 s at 20k pods regardless of NodePool count.
- **NodePool cost lands in heap and CPU.** That's why the `nodepools` levels gate on those.

  | | 1 NodePool | 10k NodePools |
  |---|---|---|
  | Heap at 100 pods | 25 MB | 275 MB |
  | Heap at 20k pods | 703 MB | 1114 MB |
  | CPU at 20k pods | 3.0 cores | 5.9 cores |

- **Startup has a cliff only where both vectors are high.** It's about 20 s across most of the grid, but 50-55 s at 3000+ NodePools × 20k pods, with 1 GB of heap at 10k × 20k.

`perf.html` has one heat map per phase and metric, with pods on x and NodePools on y:
- The shade is log-scaled, from green (the panel's lowest value) to red (its highest).
- Every cell prints its value, and hovering shows sizes and run count.
- Each panel has a table view.

## Running

```sh
OUTPUT_DIR=$(mktemp -d) TEST_SUITE=Performancev2 FOCUS=nodepools make e2etests
KARPENTER_PERF_PROVIDER=kwok-ci ...                                 # the smoke sizes and levels
KARPENTER_PERF_SIZES='{"nodepools":10000}' ...                     # override a vector for every scenario
KARPENTER_PERF_SIZES='{"topology":{"pods":5000}}' ...              # or for one scenario
KARPENTER_PERF_THRESHOLDS='{"pods/scaleOut":{"heap_mb":200}}' ...  # per-runner bounds
KARPENTER_PERF_THRESHOLDS='{"pods/scaleOut":{"pod_ready_seconds_p95":{"bound":20,"tier":"experimental"}}}' ...  # demote a flaky gate
```

Each phase writes these to `OUTPUT_DIR`:
- `<provider>_<scenario>_<phase>_<sizes>_performance_report.json`, with `schema_version`. Every gate records whether an override changed it.
- `karpenter_memory_profile_*.pb.gz`, a heap profile taken after the phase's last metrics scrape, and with `KARPENTER_PERF_CPU_PROFILE=true` `karpenter_cpu_profile_*.pb.gz`, the busiest 20 s CPU profile. Both need the controller's `ENABLE_PROFILING`. CPU profiling raised `cpu_cores` 15-21% on kind, so don't compare profiled runs to the bounds.
- A row group in `perf_summary.md`, and in `$GITHUB_STEP_SUMMARY` when set. It shows every gate's value, bound, tier and result, and the exact command to reproduce the run.

Stable breaches also print a `::error` annotation, and Experimental ones a `::warning`.

## Writing a provider plugin

A KWOK-based provider, such as the AWS provider's `./kwok`, writes a `suite_test.go` and a provider file against `perf`, and reuses `perf/scenarios` unchanged. The `Provider` fields are the seams:

| Field | What it varies |
|---|---|
| `NodeClass(s)`, `NodePool(s, nc)` | the starting objects, per scenario |
| `Sizes` | default vector sizes per scenario |
| `Shapes` | a scenario deployment's requests, keyed `"<scenario>/<deployment>"` |
| `TimeoutScale` | every wait in the scenarios, for slower real launches |
| `Metrics` | provider metrics. They gate, take overrides, calibrate and show in the inventory like core ones |
| `Capture` | extra metric family prefixes to keep in reports, on top of `perf.DefaultCapture` |
| `Sinks` | callbacks that get every phase's `Result`, for example to push to a metrics backend |
| `ReproEnv` | the provider's own env vars, such as `CLUSTER_NAME`, to include in repro lines |
| `Levels`, `Calibration` | gates and the evidence behind them |

A provider metric reads what `Capture` kept:

```go
var AWSAPICalls = perf.Metric{Name: "aws_api_calls", Kind: perf.KindResource, Unit: "calls", Upper: true,
	Measures: "AWS SDK requests the leader made during the phase.", OnBreach: "A cache stopped hitting...",
	Value: func(r *perf.Result) float64 { return float64(r.Counter("aws_sdk_go_request_total", nil)) }}
```

`perf.InventoryMain` and `perf.CalibrateMain` are the inventory and calibrate commands, so a provider's `cmd/inventory/main.go` is one call.

## Regression patches

`perf/regressions/` holds one-line mutations and clean reverts of known perf fixes, each a patch against main. Its [README](perf/regressions/README.md) has the measured table of which layer catches which regression: Go microbenchmarks, kind e2e, and v2 on EKS at default and large sizes.

The `kwok` levels come from 5 baselines and a planted regression on an EKS cluster with a tier-xl provisioned control plane. `mconc1` (concurrency forced to 1) breaches every Stable gate: `pods/scaleOut` pod_ready p95 and p99, and `nodepools/scheduling` time. Heap is Experimental. Its baselines moved 24-33% between the October and November EKS runs, and October's core reproduces the November numbers, so the environment moved rather than core. The `kwok-ci` levels come from kind capped at runner size, with 5 to 7 baselines and all 18 patches.

## Review standard

Every PR that adds or changes a perf test shows:
1. It catches a patch from `perf/regressions/`, and doesn't breach on a negative control.
2. 5 base runs with median, max and CV for each gated metric.
3. Where each bound comes from, and the regression size it lets through (bound / median - 1).
4. No Stable wall-clock gate on consolidation or drift.
5. It isn't duplicated in an open PR or an existing scenario.
6. It fails loudly on a controller restart.
7. What it costs, in minutes per run.
