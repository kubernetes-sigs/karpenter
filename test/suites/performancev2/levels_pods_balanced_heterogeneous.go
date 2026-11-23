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

// podsBalancedHeterogeneousLevels: see levels.go for the rule. Utilization is measured only until recalibrated without DaemonSet
// requests, and kwok-ci keeps only heap and CPU, the series perfv1 measured the same way. Heap and CPU stay Experimental with bounds above the
// rv2013 control, which moved them 60% like every planted regression did: that shift was the environment, not the
// patches.
var podsBalancedHeterogeneousLevels = scenarioLevels{
	KWOK: perf.Levels{
		"podsBalancedHeterogeneous/scaleOut": {
			perf.PodReady.P95.Stable(8), perf.PodReady.P99.Stable(8), perf.PodReady.Max.Experimental(8), perf.Nodes.Experimental(33), perf.HeapMB.Experimental(107), perf.CPUCores.Experimental(0.486),
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
