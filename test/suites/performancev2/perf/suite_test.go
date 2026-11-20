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
	"fmt"
	"math"
	"os"
	"testing"

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
	Name:    "pods",
	Vectors: map[perf.Vector]int{perf.VectorPods: 1000},
	Phases:  []perf.PhaseSpec{{Name: "scaleOut"}, {Name: "consolidation"}},
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
	It("lists provider metrics with their alarm text", func() {
		m := perf.Metric{Name: "api_calls", Kind: perf.KindResource, Unit: "calls", Upper: true, Measures: "API calls.", OnBreach: "Check caches."}
		p := provider(perf.Levels{"pods/scaleOut": {m.Experimental(100)}}, nil)
		p.Metrics = []perf.Metric{m}
		out, err := perf.Inventory([]perf.Scenario{scenario}, p)
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(ContainSubstring("| api_calls | resource | calls | above bound | API calls. | Check caches. |"))
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
