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

// nodepoolsDiverseLevels: see levels.go for the rule. Utilization is measured only until recalibrated without DaemonSet
// requests, and kwok-ci keeps only heap and CPU, the series perfv1 measured the same way. kwok-ci measures only: perfv1 never ran this on GitHub runners.
var nodepoolsDiverseLevels = scenarioLevels{
	KWOK: perf.Levels{
		"nodepoolsDiverse/scaleOut": {
			perf.PodReady.P95.Stable(6), perf.PodReady.P99.Stable(6), perf.PodReady.Max.Experimental(6), perf.Nodes.Experimental(5), perf.HeapMB.Experimental(89.3), perf.CPUCores.Experimental(0.814),
		},
		"nodepoolsDiverse/consolidation": {
			perf.TotalTimeSeconds.Experimental(297), perf.Nodes.Experimental(5), perf.PodsDisrupted.Experimental(14), perf.HeapMB.Experimental(82.1), perf.CPUCores.Experimental(0.287),
		},
	},
	KWOKCI: perf.Levels{
		"nodepoolsDiverse/scaleOut":      {},
		"nodepoolsDiverse/consolidation": {},
	},
}
