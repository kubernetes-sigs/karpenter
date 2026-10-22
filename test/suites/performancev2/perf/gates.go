/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Kind groups metrics by the question they answer: how long a state change took (duration), where it
// ended up (outcome), or what Karpenter spent getting there (resource).
type Kind string

const (
	KindDuration Kind = "duration"
	KindOutcome  Kind = "outcome"
	KindResource Kind = "resource"
)

type Metric struct {
	Name     string
	Kind     Kind
	Unit     string
	Upper    bool
	Measures string
	OnBreach string
	Value    func(*Result) float64
}

var (
	TotalTimeSeconds = Metric{Name: "total_time_seconds", Kind: KindDuration, Unit: "s", Upper: true,
		Measures: "Wall time of the whole phase, from its start (before the trigger) to its end state. For phases without a per-pod view (consolidation); pod-facing phases gate pod_ready_seconds instead.",
		OnBreach: "The end state arrived later. With flat cpu_cores Karpenter is waiting (batching, NodeClaim launch, registration, API throttling); with higher cpu_cores it is computing more (scheduler Solve, disruption simulation).",
		Value:    func(r *Result) float64 { return r.Total.Seconds() }}
	HeapMB = Metric{Name: "heap_mb", Kind: KindResource, Unit: "MB", Upper: true,
		Measures: "P95 of the leader's go_gc_heap_live_bytes, sampled every 2s.",
		OnBreach: "Karpenter retains more per pod, node or NodePool (caches, cluster state, per-NodePool offerings). Diff the report's counters and latency_stats against the last green run.",
		Value:    func(r *Result) float64 { return r.Resources.P95HeapMB }}
	CPUCores = Metric{Name: "cpu_cores", Kind: KindResource, Unit: "cores", Upper: true,
		Measures: "Average CPU of the leader over the phase (process_cpu_seconds_total delta over wall time).",
		OnBreach: "More work per event: scheduler Solve, disruption simulation, or extra reconciles (controller_runtime_reconcile_total in the report's counters shows which controller).",
		Value:    func(r *Result) float64 { return r.Resources.AvgCPUCores }}
	StartupSeconds = Metric{Name: "startup_seconds", Kind: KindDuration, Unit: "s", Upper: true,
		Measures: "Time from deleting the controller pods to the new leader reporting karpenter_cluster_state_synced=1.",
		OnBreach: "Startup got slower: informer sync, cluster state sync, or a startup-time pass over every object. A crashloop here is an outage at scale.",
		Value:    func(r *Result) float64 { return r.StartupSeconds }}

	CPUUtil = Metric{Name: "cpu_util", Kind: KindOutcome, Unit: "fraction",
		Measures: "Pod CPU requests over allocatable CPU, across the run's nodes at the end of the phase.",
		OnBreach: "The same pods ended up on more or bigger nodes: worse instance selection, or consolidation that stopped early. Check nodes and the NodeClaim instance types in the report.",
		Value:    func(r *Result) float64 { return r.ReservedCPUUtil }}
	MemoryUtil = Metric{Name: "memory_util", Kind: KindOutcome, Unit: "fraction",
		Measures: "Pod memory requests over allocatable memory, across the run's nodes at the end of the phase.",
		OnBreach: "The same pods ended up on more or bigger nodes: worse instance selection, or consolidation that stopped early.",
		Value:    func(r *Result) float64 { return r.ReservedMemoryUtil }}
	Nodes = Metric{Name: "nodes", Kind: KindOutcome, Unit: "nodes", Upper: true,
		Measures: "Nodes in the run's NodePools at the end of the phase.",
		OnBreach: "More nodes for the same pods: worse bin-packing, or consolidation that didn't converge.",
		Value:    func(r *Result) float64 { return float64(r.Nodes) }}
	PodsDisrupted = Metric{Name: "pods_disrupted", Kind: KindOutcome, Unit: "pods", Upper: true,
		Measures: "Pods Karpenter evicted during the phase (karpenter_pods_drained_total delta).",
		OnBreach: "More churn for the same change: extra or flapping disruption decisions, or a worse consolidation order.",
		Value:    func(r *Result) float64 { return float64(r.PodsDisrupted) }}

	// PodReady is the per-pod view of latency: each quantile is its own metric, so a provider can gate p95
	// and only warn on p99 and max, which are noisier but are what the slowest customer sees.
	PodReady = podReadyMetrics()

	// TODO: gate on scheduling latency, the mean of karpenter_scheduler_scheduling_duration_seconds over a
	// phase (it's still in the report as latency_stats). Cut for now because in 13 single runs a negative
	// control (revert #2013) moved it +42%, as much as real regressions did, so it would flake. It needs
	// repeated runs or base-vs-head on the same cluster before it can gate.
	Metrics = []Metric{TotalTimeSeconds, PodReady.P50, PodReady.P95, PodReady.P99, PodReady.Max, StartupSeconds, CPUUtil, MemoryUtil, Nodes, PodsDisrupted, HeapMB, CPUCores}
)

type QuantileMetrics struct {
	P50, P95, P99, Max Metric
}

func podReadyMetrics() QuantileMetrics {
	q := func(name, label, onBreach string, v func(Quantiles) float64) Metric {
		return Metric{Name: "pod_ready_seconds_" + name, Kind: KindDuration, Unit: "s", Upper: true,
			Measures: label + " of seconds from a pod's creation to its Ready condition, over pods created during the phase.",
			OnBreach: onBreach,
			Value:    func(r *Result) float64 { return v(r.PodReady) }}
	}
	typical := "Most pods wait longer: a systemic slowdown in batching, scheduler Solve, NodeClaim launch or registration. Compare with cpu_cores: flat means waiting, higher means computing."
	tail := "Some pods wait much longer than the rest: stragglers left for a later batch, a NodeClaim that launched late, or pods retried after a failed or incompatible scheduling decision. Noisier than p95, but it is the slowest customer's experience."
	return QuantileMetrics{
		P50: q("p50", "Median", typical, func(x Quantiles) float64 { return x.P50 }),
		P95: q("p95", "P95", typical, func(x Quantiles) float64 { return x.P95 }),
		P99: q("p99", "P99", tail, func(x Quantiles) float64 { return x.P99 }),
		Max: q("max", "Max (p100)", tail, func(x Quantiles) float64 { return x.Max }),
	}
}

// Tier decides what a breached gate does. Every new gate starts Experimental; a provider's owner
// promotes it to Stable once it has held on main (see test/suites/performancev2/README.md).
type Tier string

const (
	TierStable       Tier = "stable"
	TierExperimental Tier = "experimental"
)

type Gate struct {
	Metric Metric
	Bound  float64
	Tier   Tier
}

func (m Metric) Stable(bound float64) Gate { return Gate{Metric: m, Bound: bound, Tier: TierStable} }
func (m Metric) Experimental(bound float64) Gate {
	return Gate{Metric: m, Bound: bound, Tier: TierExperimental}
}

type GateResult struct {
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	Bound  float64 `json:"bound"`
	Upper  bool    `json:"upper"`
	Tier   Tier    `json:"tier"`
	Passed bool    `json:"passed"`
}

func (g GateResult) String() string {
	op := ">"
	if g.Upper {
		op = "<"
	}
	return fmt.Sprintf("%s = %.4g, want %s %.4g (%s)", g.Metric, g.Value, op, g.Bound, g.Tier)
}

func overrides() (map[string]map[string]float64, error) {
	o := map[string]map[string]float64{}
	inline := os.Getenv("KARPENTER_PERF_THRESHOLDS")
	if inline == "" {
		return o, nil
	}
	if err := json.NewDecoder(strings.NewReader(inline)).Decode(&o); err != nil {
		return nil, fmt.Errorf("parsing KARPENTER_PERF_THRESHOLDS: %w", err)
	}
	for id, byMetric := range o {
		for name := range byMetric {
			known := false
			for _, m := range Metrics {
				known = known || m.Name == name
			}
			if !known {
				return nil, fmt.Errorf("KARPENTER_PERF_THRESHOLDS %s: unknown metric %q", id, name)
			}
		}
	}
	return o, nil
}

func evaluate(gates []Gate, id string, r *Result) []GateResult {
	GinkgoHelper()
	o, err := overrides()
	Expect(err).ToNot(HaveOccurred())
	results := make([]GateResult, 0, len(gates))
	for _, g := range gates {
		bound := g.Bound
		if v, ok := o[id][g.Metric.Name]; ok {
			bound = v
		}
		v := g.Metric.Value(r)
		results = append(results, GateResult{
			Metric: g.Metric.Name, Value: v, Bound: bound, Upper: g.Metric.Upper, Tier: g.Tier,
			Passed: (g.Metric.Upper && v < bound) || (!g.Metric.Upper && v > bound),
		})
	}
	return results
}

func assertGates(r *Run, id string, results []GateResult) {
	GinkgoHelper()
	var failed []GateResult
	for _, g := range results {
		switch {
		case g.Passed:
			GinkgoWriter.Printf("gate ok   %s: %s\n", id, g)
		case g.Tier == TierExperimental:
			GinkgoWriter.Printf("gate WARN %s: %s\n", id, g)
			AddReportEntry("experimental gate breached", r.alarm(id, []GateResult{g}))
			fmt.Printf("::warning title=perf %s::%s\n", id, g)
		default:
			failed = append(failed, g)
		}
	}
	if len(failed) > 0 {
		Fail(r.alarm(id, failed))
	}
}
