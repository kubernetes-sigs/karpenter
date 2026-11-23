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

// podsBalancedLevels: see levels.go for the rule. Utilization is measured only until recalibrated without DaemonSet
// requests, and kwok-ci keeps only heap and CPU, the series perfv1 measured the same way.
var podsBalancedLevels = scenarioLevels{
	KWOK: perf.Levels{
		"podsBalanced/scaleOut": {
			perf.PodReady.P95.Stable(23), perf.PodReady.P99.Stable(24), perf.PodReady.Max.Experimental(24), perf.Nodes.Experimental(196), perf.HeapMB.Experimental(155), perf.CPUCores.Experimental(1.124),
		},
		"podsBalanced/consolidation": {
			perf.TotalTimeSeconds.Experimental(667), perf.Nodes.Experimental(140), perf.PodsDisrupted.Experimental(177), perf.HeapMB.Experimental(141), perf.CPUCores.Experimental(0.24),
		},
	},
	KWOKCI: perf.Levels{
		"podsBalanced/scaleOut": {
			perf.CPUCores.Experimental(0.471), perf.HeapMB.Experimental(115),
		},
		"podsBalanced/consolidation": {
			perf.CPUCores.Experimental(0.27), perf.HeapMB.Experimental(112),
		},
	},
}
