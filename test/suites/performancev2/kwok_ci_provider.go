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
// periodics. Runners are small and noisy, so sizes are small and only gross regressions fail a PR.
func KWOKCI(c env.Clients) *perf.Provider {
	p := KWOK(c)
	p.Name = "kwok-ci"
	p.Summary = "Per-PR smoke and periodics on a GitHub Actions kind runner. Stable only for gross regressions."
	p.Sizes = map[string]map[perf.Vector]int{
		"pods":      {perf.VectorPods: 300},
		"nodepools": {perf.VectorNodePools: 100, perf.VectorPods: 100},
		"topology":  {perf.VectorPods: 300},
	}
	// Bounds start from calibration/kwok-ci.json (kind capped at runner size) and allow for GitHub runners, which ran
	// pod latency up to 1.4x slower and consolidation up to 1.6x slower than the calibration host. Stable gates sit
	// at least 2x above every observed baseline and only where a planted regression breaches them; Experimental
	// gates sit about 1.3x above the slowest baseline seen on either host.
	p.Calibration = perf.MustLoadCalibration(kwokCICalibration)
	p.Levels = perf.Levels{
		"pods/scaleOut": {
			perf.PodReady.P95.Experimental(36), perf.PodReady.P99.Experimental(38), perf.PodReady.Max.Experimental(38),
			perf.Nodes.Experimental(57), perf.HeapMB.Experimental(45.9), perf.CPUCores.Experimental(0.271),
		},
		"pods/consolidation": {
			perf.TotalTimeSeconds.Experimental(700), perf.Nodes.Experimental(53),
			perf.HeapMB.Experimental(42.8), perf.CPUCores.Experimental(0.081),
		},
		"nodepools/scheduling": {
			perf.TotalTimeSeconds.Stable(16), perf.PodReady.P95.Experimental(9), perf.HeapMB.Experimental(29.4), perf.CPUCores.Experimental(0.213),
		},
		"nodepools/startup": {
			perf.StartupSeconds.Experimental(5), perf.HeapMB.Experimental(27),
		},
		"topology/scaleOut": {
			perf.PodReady.P95.Stable(80), perf.PodReady.P99.Experimental(80), perf.PodReady.Max.Experimental(80),
			perf.Nodes.Experimental(187), perf.HeapMB.Experimental(89.3), perf.CPUCores.Experimental(0.432),
		},
	}
	return p
}
