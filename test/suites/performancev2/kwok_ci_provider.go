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

package performancev2

import (
	_ "embed"

	"sigs.k8s.io/karpenter/test/suites/performancev2/env"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

//go:embed calibration/kwok-ci.json
var kwokCICalibration []byte

// KWOKCI is the core KWOK provider on a GitHub Actions kind runner, for the per-PR smoke test and the
// periodics. Runners are small and noisy, so sizes are small and only gross regressions gate.
func KWOKCI(c env.Clients) *perf.Provider {
	p := KWOK(c)
	p.Name = "kwok-ci"
	p.Summary = "Per-PR smoke and periodics on a GitHub Actions kind runner. Stable only for gross regressions."
	p.Sizes = map[string]map[perf.Vector]int{
		"pods":      {perf.VectorPods: 300},
		"nodepools": {perf.VectorNodePools: 100, perf.VectorPods: 100},
		"topology":  {perf.VectorPods: 300},
	}
	// Bounds are 2x the max of 5 baselines from calibration/kwok-ci.json. Only pod_ready p95 is Stable, where
	// concurrency forced to 1 (mconc1) more than triples it and the rv2013 control doesn't move it.
	p.Calibration = perf.MustLoadCalibration(kwokCICalibration)
	p.Levels = perf.Levels{
		"pods/scaleOut": {
			perf.PodReady.P95.Stable(48), perf.PodReady.P99.Experimental(52), perf.PodReady.Max.Experimental(54),
			perf.Nodes.Experimental(102), perf.HeapMB.Experimental(72), perf.CPUCores.Experimental(0.43),
		},
		"pods/consolidation": {
			perf.TotalTimeSeconds.Experimental(1183), perf.Nodes.Experimental(76), perf.HeapMB.Experimental(76),
		},
		"nodepools/scheduling": {
			perf.TotalTimeSeconds.Experimental(20), perf.PodReady.P95.Experimental(16), perf.HeapMB.Experimental(63), perf.CPUCores.Experimental(0.42),
		},
		"nodepools/startup": {
			perf.StartupSeconds.Experimental(43), perf.HeapMB.Experimental(51),
		},
		"topology/scaleOut": {
			perf.PodReady.P95.Stable(66), perf.PodReady.P99.Experimental(82), perf.PodReady.Max.Experimental(82),
			perf.Nodes.Experimental(322), perf.HeapMB.Experimental(165), perf.CPUCores.Experimental(0.73),
		},
	}
	return p
}
