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

package performance

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type Metric struct {
	Name  string
	Upper bool
	Value func(*PerformanceReport) float64
}

var (
	TotalTimeMinutes = Metric{Name: "total_time_minutes", Upper: true, Value: func(r *PerformanceReport) float64 { return r.TotalTime.Minutes() }}
	HeapMB           = Metric{Name: "heap_mb", Upper: true, Value: func(r *PerformanceReport) float64 { return r.KarpenterP95HeapMB }}
	CPUCores         = Metric{Name: "cpu_cores", Upper: true, Value: func(r *PerformanceReport) float64 { return r.KarpenterAvgCPUCores }}
	CPUCoresP95      = Metric{Name: "cpu_cores_p95", Upper: true, Value: func(r *PerformanceReport) float64 { return r.KarpenterP95CPUCores }}
	CPUUtil          = Metric{Name: "cpu_util", Value: func(r *PerformanceReport) float64 { return r.TotalReservedCPUUtil }}
	MemoryUtil       = Metric{Name: "memory_util", Value: func(r *PerformanceReport) float64 { return r.TotalReservedMemoryUtil }}

	metrics = []Metric{TotalTimeMinutes, HeapMB, CPUCores, CPUCoresP95, CPUUtil, MemoryUtil}
)

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

func Stable(m Metric, bound float64) Gate { return Gate{Metric: m, Bound: bound, Tier: TierStable} }
func Experimental(m Metric, bound float64) Gate {
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

var gates = map[string][]Gate{
	"basic/scaleOut": {
		Stable(TotalTimeMinutes, 2), Stable(CPUUtil, 0.53), Stable(MemoryUtil, 0.65), Stable(HeapMB, 120), Stable(CPUCores, 0.50),
	},
	"basic/consolidation": {
		Experimental(TotalTimeMinutes, 20), Stable(HeapMB, 140), Stable(CPUCores, 0.25),
	},
	"drift/initial": {
		Stable(TotalTimeMinutes, 5), Stable(HeapMB, 190), Stable(CPUCores, 0.70),
	},
	"drift/drift": {
		Stable(TotalTimeMinutes, 50), Stable(HeapMB, 250), Stable(CPUCores, 1.05),
	},
	"doNotDisrupt/scaleOut": {
		Stable(TotalTimeMinutes, 4), Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 340), Stable(CPUCores, 1.10),
	},
	"doNotDisrupt/consolidation": {
		Experimental(TotalTimeMinutes, 35), Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 320), Experimental(CPUCores, 0.80),
	},
	"hostNameSpreading/scaleOut": {
		Stable(TotalTimeMinutes, 5), Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 360), Stable(CPUCores, 1.15),
	},
	"hostNameSpreading/consolidation": {
		Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 350), Experimental(CPUCores, 1.00),
	},
	"hostNameSpreadingXL/scaleOut": {
		Stable(TotalTimeMinutes, 35), Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 720), Stable(CPUCores, 1.40),
	},
	"hostNameSpreadingXL/consolidation": {
		Experimental(TotalTimeMinutes, 35), Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 730), Stable(CPUCores, 1.70),
	},
	"interference/scaleOut": {
		Stable(TotalTimeMinutes, 5), Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 310), Stable(CPUCores, 0.90),
	},
	"interference/interference": {
		Stable(TotalTimeMinutes, 10), Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 590), Stable(CPUCores, 1.30),
	},
	"interference/consolidation": {
		Stable(TotalTimeMinutes, 25), Stable(CPUUtil, 0.38), Stable(MemoryUtil, 0.40), Stable(HeapMB, 540), Stable(CPUCores, 0.80),
	},
	"wideDeployments/scaleOut": {
		Stable(TotalTimeMinutes, 5), Stable(CPUUtil, 0.20), Stable(MemoryUtil, 0.20), Stable(HeapMB, 150), Stable(CPUCores, 0.70),
	},
	"wideDeployments/consolidation": {
		Experimental(TotalTimeMinutes, 40), Stable(CPUUtil, 0.20), Stable(MemoryUtil, 0.20), Stable(HeapMB, 210), Experimental(CPUCores, 0.30),
	},
	"manyNodePools/scaleOut": {
		Stable(HeapMB, 60), Stable(CPUCores, 0.80), Stable(CPUCoresP95, 0.80), Stable(TotalTimeMinutes, 5),
	},
	"manyNodePools/consolidation": {
		Stable(HeapMB, 90), Stable(CPUCores, 0.40), Stable(CPUCoresP95, 1.20), Experimental(TotalTimeMinutes, 15),
	},
	"manyNodePoolsTargeted/scaleOut": {
		Stable(HeapMB, 170), Stable(CPUCores, 1.50), Stable(CPUCoresP95, 2.00),
	},
	"manyNodePoolsTargeted/consolidation": {
		Stable(HeapMB, 180), Stable(CPUCores, 2.00), Stable(CPUCoresP95, 2.50), Experimental(TotalTimeMinutes, 20),
	},
	"balancedBasic/scaleOut":              {Experimental(HeapMB, 130), Experimental(CPUCores, 0.50)},
	"balancedBasic/consolidation":         {Experimental(HeapMB, 140), Experimental(CPUCores, 0.30)},
	"balancedChurn/scaleOut":              {Experimental(HeapMB, 60), Experimental(CPUCores, 0.25)},
	"balancedChurn/consolidation":         {Experimental(HeapMB, 60), Experimental(CPUCores, 0.10)},
	"balancedHeterogeneous/scaleOut":      {Experimental(HeapMB, 60), Experimental(CPUCores, 0.25)},
	"balancedHeterogeneous/consolidation": {Experimental(HeapMB, 60), Experimental(CPUCores, 0.10)},
}

var (
	overridesOnce sync.Once
	overrides     map[string]map[string]float64
	overridesErr  error
)

func loadOverrides() (map[string]map[string]float64, error) {
	overridesOnce.Do(func() {
		overrides = map[string]map[string]float64{}
		inline := os.Getenv("KARPENTER_PERF_THRESHOLDS")
		if inline == "" {
			return
		}
		if overridesErr = json.NewDecoder(strings.NewReader(inline)).Decode(&overrides); overridesErr != nil {
			overridesErr = fmt.Errorf("parsing performance threshold overrides: %w", overridesErr)
			return
		}
		overridesErr = validateOverrides(overrides)
	})
	return overrides, overridesErr
}

func validateOverrides(o map[string]map[string]float64) error {
	for id, byMetric := range o {
		if _, ok := gates[id]; !ok {
			return fmt.Errorf("performance threshold override for unknown phase %q", id)
		}
		for name := range byMetric {
			known := false
			for _, m := range metrics {
				known = known || m.Name == name
			}
			if !known {
				return fmt.Errorf("performance threshold override %s has unknown metric %q", id, name)
			}
		}
	}
	return nil
}

func evaluate(id string, r *PerformanceReport) ([]GateResult, error) {
	gs, ok := gates[id]
	if !ok {
		return nil, fmt.Errorf("no gates registered for phase %q", id)
	}
	o, err := loadOverrides()
	if err != nil {
		return nil, err
	}
	results := make([]GateResult, 0, len(gs))
	for _, g := range gs {
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
	return results, nil
}

func (g GateResult) String() string {
	op := ">"
	if g.Upper {
		op = "<"
	}
	return fmt.Sprintf("%s = %.4g, want %s %.4g (%s)", g.Metric, g.Value, op, g.Bound, g.Tier)
}

func ExpectGates(r *PerformanceReport, id string) {
	GinkgoHelper()
	results, err := evaluate(id, r)
	Expect(err).ToNot(HaveOccurred())
	var failed []string
	for _, g := range results {
		switch {
		case g.Passed:
			GinkgoWriter.Printf("gate ok   %s: %s\n", id, g)
		case g.Tier == TierExperimental:
			GinkgoWriter.Printf("gate WARN %s: %s\n", id, g)
			AddReportEntry("experimental gate breached", fmt.Sprintf("%s: %s", id, g))
			fmt.Printf("::warning title=perf %s::%s\n", id, g)
		default:
			failed = append(failed, g.String())
		}
	}
	Expect(failed).To(BeEmpty(), "%s breached stable performance gates", id)
}
