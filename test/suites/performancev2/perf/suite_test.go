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

package perf_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/kwok/apis"
	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	"sigs.k8s.io/karpenter/test/suites/performancev2/env"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

func TestPerf(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Perf")
}

var scenario = perf.Scenario{
	Name:     "pods",
	Question: "How fast do pods become Ready?",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 1000},
	Phases:   []perf.PhaseSpec{{Name: "scaleOut"}, {Name: "consolidation"}},
}

func evidence(median, cv float64, variants map[string]float64) perf.MetricEvidence {
	return perf.MetricEvidence{N: 5, Median: median, Min: median, Max: median, CV: cv, Variants: variants}
}

func gates(gs ...perf.Gate) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, fmt.Sprintf("%s %s %g", g.Metric.Name, g.Tier, g.Bound))
	}
	return out
}

func provider(levels perf.Levels, ev map[string]map[string]perf.MetricEvidence) *perf.Provider {
	return &perf.Provider{Name: "test", Levels: levels, Calibration: &perf.Calibration{Phases: ev}}
}

var _ = Describe("Levels.With", func() {
	base := perf.Levels{
		"pods/scaleOut":      {perf.HeapMB.Stable(100), perf.CPUCores.Experimental(1)},
		"pods/consolidation": {perf.HeapMB.Experimental(100)},
	}
	It("replaces a gate on the same metric and keeps the rest", func() {
		got := base.With(perf.Levels{"pods/scaleOut": {perf.HeapMB.Experimental(120)}})
		Expect(gates(got["pods/scaleOut"]...)).To(ConsistOf(gates(perf.HeapMB.Experimental(120), perf.CPUCores.Experimental(1))))
		Expect(gates(got["pods/consolidation"]...)).To(Equal(gates(base["pods/consolidation"]...)))
	})
	It("drops a gate with Metric.Off", func() {
		got := base.With(perf.Levels{"pods/scaleOut": {perf.CPUCores.Off()}})
		Expect(gates(got["pods/scaleOut"]...)).To(ConsistOf(gates(perf.HeapMB.Stable(100))))
	})
	It("turns a phase off with perf.Off, and adds new phases", func() {
		got := base.With(perf.Levels{"pods/consolidation": perf.Off, "nodepools/startup": {perf.StartupSeconds.Experimental(30)}})
		Expect(gates(got["pods/consolidation"]...)).To(Equal(gates(perf.Off...)))
		Expect(gates(got["nodepools/startup"]...)).To(ConsistOf(gates(perf.StartupSeconds.Experimental(30))))
	})
	It("doesn't change the levels it starts from", func() {
		_ = base.With(perf.Levels{"pods/scaleOut": {perf.HeapMB.Off()}})
		Expect(base["pods/scaleOut"]).To(HaveLen(2))
	})
})

var _ = Describe("power model", func() {
	It("puts MDE50 at the median's distance from the bound", func() {
		Expect(evidence(10, 0.1, nil).MDE(perf.HeapMB, 12, 0.5)).To(BeNumerically("~", 0.2, 1e-9))
	})
	It("needs a bigger regression for 80% power", func() {
		e := evidence(10, 0.1, nil)
		Expect(e.MDE(perf.HeapMB, 12, 0.8)).To(BeNumerically("~", 12/(10-0.8416*1)-1, 1e-3))
	})
	It("works from below for metrics that breach under the bound", func() {
		Expect(evidence(0.9, 0, nil).MDE(perf.CPUUtil, 0.81, 0.5)).To(BeNumerically("~", 0.1, 1e-9))
	})
	It("estimates the false positive rate from the CV", func() {
		Expect(evidence(10, 0.1, nil).FPR(perf.HeapMB, 10+2.3263)).To(BeNumerically("~", 0.01, 1e-4))
		Expect(evidence(10, 0.1, nil).FPR(perf.HeapMB, 10)).To(BeNumerically("~", 0.5, 1e-9))
	})
	It("sizes repeats for a base-vs-head comparison", func() {
		z := 2.3263 + 0.8416
		Expect(perf.RunsFor(0.05, 0.10, 0.01, 0.8)).To(Equal(int(math.Ceil(2 * z * z * 0.25))))
		Expect(perf.RunsFor(0.20, 0.05, 0.01, 0.8)).To(BeNumerically(">", perf.RunsFor(0.05, 0.05, 0.01, 0.8)))
	})
})

var _ = Describe("Inventory", func() {
	ev := map[string]map[string]perf.MetricEvidence{"pods/scaleOut": {"heap_mb": evidence(60, 0.04, map[string]float64{"reg": 90})}}
	It("accepts a Stable gate with evidence, and measures unlisted phases", func() {
		out, err := perf.Inventory([]perf.Scenario{scenario}, provider(perf.Levels{"pods/scaleOut": {perf.HeapMB.Stable(75)}}, ev))
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(ContainSubstring("| pods/scaleOut | resource | heap_mb | stable | < 75 MB | 5 | 60 | 4% | 25% |"))
		Expect(out).To(ContainSubstring("| pods/consolidation | - | - | measured |"))
	})
	It("shows an Off phase and every later phase as off", func() {
		out, err := perf.Inventory([]perf.Scenario{scenario}, provider(perf.Levels{"pods/scaleOut": perf.Off}, ev))
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(ContainSubstring("| pods/scaleOut | - | - | off |"))
		Expect(out).To(ContainSubstring("| pods/consolidation | - | - | off |"))
	})
	It("rejects a Stable gate with a high false positive rate", func() {
		_, err := perf.Inventory([]perf.Scenario{scenario}, provider(perf.Levels{"pods/scaleOut": {perf.HeapMB.Stable(62)}}, ev))
		Expect(err).To(MatchError(ContainSubstring("false positive rate")))
	})
	It("rejects a Stable gate no planted regression breaches", func() {
		_, err := perf.Inventory([]perf.Scenario{scenario}, provider(perf.Levels{"pods/scaleOut": {perf.HeapMB.Stable(95)}}, ev))
		Expect(err).To(MatchError(ContainSubstring("no planted regression breaches")))
	})
	It("runs every scenario by default and skips one whose Requires the provider doesn't meet", func() {
		needs := scenario
		needs.Name = "needs"
		needs.Requires = func(p *perf.Provider) error {
			if len(p.Catalog.Families) < 2 {
				return fmt.Errorf("needs two Catalog.Families")
			}
			return nil
		}
		all := []perf.Scenario{scenario, needs}
		p := provider(perf.Levels{}, ev)
		Expect(perf.Validate(p, all)).To(Succeed())
		Expect(perf.EnabledScenarios(p, all)).To(Equal([]string{"pods"}))
		p.Catalog.Families = []string{"c", "m"}
		Expect(perf.EnabledScenarios(p, all)).To(Equal([]string{"pods", "needs"}))
	})
	It("lets a provider opt out of a scenario with a reason", func() {
		p := provider(perf.Levels{}, ev)
		p.Skip = map[string]string{"pods": "no spot capacity in this provider"}
		Expect(perf.Validate(p, []perf.Scenario{scenario})).To(Succeed())
		Expect(perf.EnabledScenarios(p, []perf.Scenario{scenario})).To(BeEmpty())
		p.Skip = map[string]string{"pods": ""}
		Expect(perf.Validate(p, []perf.Scenario{scenario})).To(MatchError(ContainSubstring(`skips "pods" without a reason`)))
		p.Skip = map[string]string{"podz": "typo"}
		Expect(perf.Validate(p, []perf.Scenario{scenario})).To(MatchError(ContainSubstring(`skips "podz", which no scenario declares`)))
	})
	It("requires every scenario to state its customer question and a declared group", func() {
		noQ, orphan := scenario, scenario
		noQ.Name, noQ.Question = "noQuestion", ""
		orphan.Name, orphan.Group = "orphan", "missing"
		err := perf.Validate(provider(perf.Levels{}, nil), []perf.Scenario{scenario, noQ, orphan})
		Expect(err).To(MatchError(ContainSubstring("scenario noQuestion has no Question")))
		Expect(err).To(MatchError(ContainSubstring(`scenario orphan is in group "missing"`)))
	})
	It("lists provider metrics with their alarm text", func() {
		m := perf.Metric{Name: "api_calls", Kind: perf.KindResource, Unit: "calls", Upper: true, Measures: "API calls.", OnBreach: "Check caches."}
		p := provider(perf.Levels{"pods/scaleOut": {m.Experimental(100)}}, nil)
		p.Metrics = []perf.Metric{m}
		out, err := perf.Inventory([]perf.Scenario{scenario}, p)
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(ContainSubstring("| api_calls | resource | calls | above bound | API calls. | Check caches. |"))
	})
	It("measures, rather than gates, a run at sizes the levels weren't calibrated at", func() {
		p := provider(perf.Levels{}, nil)
		Expect(perf.Resized(p, &scenario, map[perf.Vector]int{perf.VectorPods: 1000})).To(BeFalse())
		Expect(perf.Resized(p, &scenario, map[perf.Vector]int{perf.VectorPods: 5000})).To(BeTrue())
		p.Sizes = map[string]map[perf.Vector]int{"pods": {perf.VectorPods: 300}}
		Expect(perf.Resized(p, &scenario, map[perf.Vector]int{perf.VectorPods: 300})).To(BeFalse(), "the provider's own Sizes are what it calibrated at")
	})
	It("sizes one scenario from KARPENTER_PERF_SIZES", func() {
		DeferCleanup(os.Unsetenv, "KARPENTER_PERF_SIZES")
		Expect(os.Setenv("KARPENTER_PERF_SIZES", `{"pods":{"pods":7},"other":{"pods":9}}`)).To(Succeed())
		out, err := perf.Inventory([]perf.Scenario{scenario}, provider(perf.Levels{"pods/scaleOut": {}}, nil))
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(ContainSubstring("**pods** (test: pods=7)"))
	})
})

var _ = Describe("Result accessors", func() {
	r := &perf.Result{
		Counters: map[string]uint64{"aws_sdk_go_request_total{action=DescribeSubnets,code=200,service=EC2}": 3, "aws_sdk_go_request_total{action=GetParameter,code=200,service=SSM}": 2},
		Gauges:   map[string]float64{"go_goroutines": 40},
		LatencyStats: map[string]perf.HistogramStats{
			"x{a=1}": {MetricName: "x", Labels: map[string]string{"a": "1"}, Count: 5, P99: 2},
			"x{a=2}": {MetricName: "x", Labels: map[string]string{"a": "2"}, Count: 9, P99: 3},
		},
	}
	It("sums counters over matching series", func() {
		Expect(r.Counter("aws_sdk_go_request_total", nil)).To(BeEquivalentTo(5))
		Expect(r.Counter("aws_sdk_go_request_total", map[string]string{"service": "EC2"})).To(BeEquivalentTo(3))
	})
	It("reads gauges and the busiest matching histogram", func() {
		Expect(r.Gauge("go_goroutines", nil)).To(Equal(40.0))
		Expect(r.Histogram("x", nil).P99).To(Equal(3.0))
		Expect(r.Histogram("x", map[string]string{"a": "1"}).P99).To(Equal(2.0))
	})
	It("lists every matching histogram series", func() {
		Expect(r.Histograms("x", nil)).To(HaveLen(2))
		Expect(r.Histograms("x", map[string]string{"a": "2"})).To(ConsistOf(HaveField("Count", BeEquivalentTo(9))))
		Expect(r.Histograms("y", nil)).To(BeEmpty())
	})
})

var _ = Describe("env.NodePool", func() {
	It("references a typed NodeClass, whose TypeMeta is empty", func() {
		nc := &v1alpha1.KWOKNodeClass{}
		nc.SetName("typed")
		ref := env.NodePool(nc).Spec.Template.Spec.NodeClassRef
		Expect(ref.Kind).To(Equal("KWOKNodeClass"))
		Expect(ref.Group).To(Equal(apis.Group))
		Expect(ref.Name).To(Equal("typed"))
	})
	It("references an unstructured NodeClass", func() {
		ref := env.NodePool(env.KWOKNodeClass()).Spec.Template.Spec.NodeClassRef
		Expect(ref.Kind).To(Equal("KWOKNodeClass"))
	})
})

var _ = Describe("Calibration rule", func() {
	It("bounds above the larger of 1.2x the slowest sample and median + 2.33 sd, counting controls as samples", func() {
		e := perf.MetricEvidence{N: 5, Median: 100, Min: 95, Max: 110, CV: 0.02}
		Expect(perf.SuggestBound(perf.HeapMB, e)).To(Equal(132.0))
		e.Controls = map[string]float64{"rv2013": 150}
		Expect(perf.SuggestBound(perf.HeapMB, e)).To(Equal(180.0))
		Expect(perf.SuggestBound(perf.HeapMB, perf.MetricEvidence{N: 5, Median: 100, Min: 90, Max: 110, CV: 0.5})).To(Equal(217.0))
	})
	It("bounds utilization below the smaller of the lowest sample / 1.15 and median - 2.33 sd", func() {
		Expect(perf.SuggestBound(perf.CPUUtil, perf.MetricEvidence{N: 5, Median: 0.8, Min: 0.79, Max: 0.81, CV: 0.01})).To(Equal(0.687))
	})
	It("keeps pod latency in whole seconds, at least 1.5x the slowest sample, and counts in whole units", func() {
		Expect(perf.SuggestBound(perf.PodReady.P95, perf.MetricEvidence{N: 5, Median: 5, Min: 5, Max: 5})).To(Equal(8.0))
		Expect(perf.SuggestBound(perf.PodReady.Max, perf.MetricEvidence{N: 5, Median: 5, Min: 5, Max: 6})).To(Equal(8.0))
		Expect(perf.SuggestBound(perf.Nodes, perf.MetricEvidence{N: 5, Median: 100, Min: 100, Max: 101})).To(Equal(122.0))
	})
	It("suggests Stable only with evidence and a catch by 10%, never on Experimental-only metrics or settle wall time", func() {
		ev := func(variants map[string]float64) perf.MetricEvidence {
			return perf.MetricEvidence{N: 5, Median: 10, Min: 10, Max: 10, CV: 0.01, Variants: variants, Controls: map[string]float64{"rv2013": 10}}
		}
		c := &perf.Calibration{
			Kinds: map[string]string{"s/scaleOut": "scale-out", "s/consolidation": "consolidation"},
			Phases: map[string]map[string]perf.MetricEvidence{
				"s/scaleOut": {"pod_ready_seconds_p95": ev(map[string]float64{"big": 100}), "pod_ready_seconds_p99": ev(map[string]float64{"thin": 15.5}),
					"pod_ready_seconds_max": ev(map[string]float64{"big": 100}), "heap_mb": ev(map[string]float64{"big": 100})},
				"s/consolidation": {"total_time_seconds": ev(map[string]float64{"big": 100})},
			},
		}
		tiers := map[string]string{}
		for id, ss := range perf.SuggestLevels(c, "s") {
			for _, s := range ss {
				tiers[id+" "+s.Gate.Metric.Name] = string(s.Gate.Tier) + " " + s.Why
			}
		}
		Expect(tiers).To(Equal(map[string]string{
			"s/scaleOut pod_ready_seconds_p95":   "stable ",
			"s/scaleOut pod_ready_seconds_p99":   "experimental no planted regression breaches it by 10%",
			"s/scaleOut pod_ready_seconds_max":   "experimental pod_ready_seconds_max is Experimental only",
			"s/scaleOut heap_mb":                 "experimental heap_mb is Experimental only",
			"s/consolidation total_time_seconds": "experimental wall time of a consolidation phase",
		}))
		Expect(perf.FormatLevels(perf.SuggestLevels(c, "s"))).To(ContainSubstring(`"s/consolidation": {` + "\n\t\tperf.TotalTimeSeconds.Experimental(12),\n\t},"))
	})
	It("never counts a negative control as a catch, and fails a Stable bound a control breaches", func() {
		e := perf.MetricEvidence{N: 5, Median: 60, Min: 58, Max: 62, CV: 0.02, Variants: map[string]float64{"reg": 90}, Controls: map[string]float64{"rv2013": 80}}
		p := provider(perf.Levels{"pods/scaleOut": {perf.HeapMB.Stable(75)}}, map[string]map[string]perf.MetricEvidence{"pods/scaleOut": {"heap_mb": e}})
		Expect(perf.CheckEvidence(p)).To(MatchError(ContainSubstring("negative control rv2013 breaches (80)")))
		e.Variants = nil
		p = provider(perf.Levels{"pods/scaleOut": {perf.HeapMB.Stable(85)}}, map[string]map[string]perf.MetricEvidence{"pods/scaleOut": {"heap_mb": e}})
		Expect(perf.CheckEvidence(p)).To(MatchError(ContainSubstring("no planted regression breaches")))
	})
})

var _ = Describe("Calibration files", func() {
	It("splits by scenario and merges back with per-scenario provenance", func() {
		c := &perf.Calibration{Provider: "kwok", Source: "eks", Baseline: "abc",
			Phases: map[string]map[string]perf.MetricEvidence{"a/x": {"heap_mb": {N: 5}}, "b/y": {"heap_mb": {N: 3}}},
			Kinds:  map[string]string{"a/x": "scale-out"}}
		fsys := fstest.MapFS{}
		for name, sc := range c.ByScenario() {
			b, err := json.Marshal(sc)
			Expect(err).ToNot(HaveOccurred())
			fsys["cal/"+name+".json"] = &fstest.MapFile{Data: b}
		}
		Expect(fsys).To(HaveLen(2))
		merged := perf.MustLoadCalibrations(fsys, "cal/*.json")
		Expect(merged.Phases).To(HaveKey("a/x"))
		Expect(merged.Phases).To(HaveKey("b/y"))
		Expect(merged.Kinds).To(Equal(map[string]string{"a/x": "scale-out"}))
		Expect(merged.Provenance()).To(HaveKeyWithValue("a", ContainSubstring("eks, baseline abc")))
	})
	It("loads a provider's single file alongside per-scenario files, and rejects a phase in both", func() {
		file := func(c perf.Calibration) *fstest.MapFile {
			b, err := json.Marshal(c)
			Expect(err).ToNot(HaveOccurred())
			return &fstest.MapFile{Data: b}
		}
		fsys := fstest.MapFS{
			"cal/kwok.json":       file(perf.Calibration{Source: "old", Phases: map[string]map[string]perf.MetricEvidence{"pods/scaleOut": {"heap_mb": {N: 5}}}}),
			"cal/kwok/drift.json": file(perf.Calibration{Source: "new", Phases: map[string]map[string]perf.MetricEvidence{"drift/drift": {"heap_mb": {N: 5}}}}),
		}
		c := perf.MustLoadCalibrations(fsys, "cal/kwok.json", "cal/kwok/*.json")
		Expect(c.Phases).To(HaveLen(2))
		Expect(c.Provenance()).To(HaveKeyWithValue("pods", HavePrefix("old,")))
		Expect(c.Provenance()).To(HaveKeyWithValue("drift", HavePrefix("new,")))
		fsys["cal/kwok/pods.json"] = file(perf.Calibration{Source: "new", Phases: map[string]map[string]perf.MetricEvidence{"pods/scaleOut": {"heap_mb": {N: 5}}}})
		Expect(func() { perf.MustLoadCalibrations(fsys, "cal/kwok.json", "cal/kwok/*.json") }).To(PanicWith(ContainSubstring("pods/scaleOut is in both")))
	})
	It("keeps controls apart from planted regressions", func() {
		dir := GinkgoT().TempDir()
		write := func(run string, heap float64) {
			Expect(os.MkdirAll(filepath.Join(dir, run), 0o700)).To(Succeed())
			b, _ := json.Marshal(perf.Result{Provider: "test", ID: "pods/scaleOut", Kind: "scale-out", Resources: perf.ResourceStats{P95HeapMB: heap}})
			Expect(os.WriteFile(filepath.Join(dir, run, "r_performance_report.json"), b, 0o600)).To(Succeed())
		}
		write("base-1", 60)
		write("base-2", 62)
		write("reg-rv2013", 61)
		write("m959", 90)
		c, err := perf.Calibrate(dir, &perf.Provider{Name: "test"}, "here", perf.DefaultControls...)
		Expect(err).ToNot(HaveOccurred())
		e := c.Phases["pods/scaleOut"]["heap_mb"]
		Expect(e.Variants).To(Equal(map[string]float64{"m959": 90}))
		Expect(e.Controls).To(Equal(map[string]float64{"rv2013": 61}))
		Expect(c.Kinds).To(HaveKeyWithValue("pods/scaleOut", "scale-out"))
	})
})
