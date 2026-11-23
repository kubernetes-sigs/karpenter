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

// podsBalancedChurnLevels: see levels.go for the rule. Utilization is measured only until recalibrated without DaemonSet
// requests, and kwok-ci keeps only heap and CPU, the series perfv1 measured the same way.
var podsBalancedChurnLevels = scenarioLevels{
	KWOK: perf.Levels{
		"podsBalancedChurn/scaleOut": {
			perf.PodReady.P95.Stable(8), perf.PodReady.P99.Stable(8), perf.PodReady.Max.Experimental(8), perf.Nodes.Experimental(29), perf.HeapMB.Experimental(135), perf.CPUCores.Experimental(0.528),
		},
		"podsBalancedChurn/consolidation": {
			perf.TotalTimeSeconds.Experimental(473), perf.Nodes.Experimental(16), perf.PodsDisrupted.Experimental(146), perf.HeapMB.Experimental(121), perf.CPUCores.Experimental(0.139),
		},
	},
	KWOKCI: perf.Levels{
		"podsBalancedChurn/scaleOut": {
			perf.CPUCores.Experimental(0.169), perf.HeapMB.Experimental(36.2),
		},
		"podsBalancedChurn/consolidation": {
			perf.CPUCores.Experimental(0.036), perf.HeapMB.Experimental(39.3),
		},
	},
}
