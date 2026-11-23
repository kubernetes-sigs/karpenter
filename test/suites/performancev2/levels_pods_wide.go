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

// podsWideLevels: see levels.go for the rule. Utilization is measured only until recalibrated without DaemonSet
// requests, and kwok-ci keeps only heap and CPU, the series perfv1 measured the same way.
var podsWideLevels = scenarioLevels{
	KWOK: perf.Levels{
		"podsWide/scaleOut": {
			perf.PodReady.P95.Stable(23), perf.PodReady.P99.Stable(26), perf.PodReady.Max.Experimental(26), perf.Nodes.Experimental(198), perf.HeapMB.Experimental(192), perf.CPUCores.Experimental(0.992),
		},
		"podsWide/consolidation": {
			perf.TotalTimeSeconds.Experimental(1108), perf.Nodes.Experimental(129), perf.PodsDisrupted.Experimental(252), perf.HeapMB.Experimental(170), perf.CPUCores.Experimental(0.373),
		},
	},
	KWOKCI: perf.Levels{
		"podsWide/scaleOut": {
			perf.CPUCores.Experimental(0.71), perf.HeapMB.Experimental(155),
		},
		"podsWide/consolidation": {
			perf.CPUCores.Experimental(0.48), perf.HeapMB.Experimental(146),
		},
	},
}
