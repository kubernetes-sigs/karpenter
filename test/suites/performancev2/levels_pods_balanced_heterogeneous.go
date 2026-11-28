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

import "sigs.k8s.io/karpenter/test/suites/performancev2/perf"

// podsBalancedHeterogeneousLevels: Experimental starting bounds; calibrate before promoting any to Stable.
var podsBalancedHeterogeneousLevels = scenarioLevels{
	KWOK: perf.Levels{
		"podsBalancedHeterogeneous/scaleOut": {
			perf.PodReady.P95.Experimental(8), perf.PodReady.P99.Experimental(8), perf.PodReady.Max.Experimental(8), perf.Nodes.Experimental(33), perf.HeapMB.Experimental(107), perf.CPUCores.Experimental(0.486),
		},
		"podsBalancedHeterogeneous/consolidation": {
			perf.TotalTimeSeconds.Experimental(513), perf.Nodes.Experimental(20), perf.PodsDisrupted.Experimental(101), perf.HeapMB.Experimental(93.6), perf.CPUCores.Experimental(0.089),
		},
	},
	KWOKCI: perf.Levels{
		"podsBalancedHeterogeneous/scaleOut": {
			perf.CPUCores.Experimental(0.179), perf.HeapMB.Experimental(37.5),
		},
		"podsBalancedHeterogeneous/consolidation": {
			perf.CPUCores.Experimental(0.053), perf.HeapMB.Experimental(44),
		},
	},
}
