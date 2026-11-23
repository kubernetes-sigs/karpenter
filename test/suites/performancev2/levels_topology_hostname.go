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

// topologyHostnameLevels: see levels.go for the rule. Utilization is measured only until recalibrated without DaemonSet
// requests, and kwok-ci keeps only heap and CPU, the series perfv1 measured the same way. No Stable gates: mconc1 makes scaleOut time out, which fails the
// spec but leaves no report to count. kwok runs perfv1's XL size (kwok_provider.go Sizes).
var topologyHostnameLevels = scenarioLevels{
	KWOK: perf.Levels{
		"topologyHostname/scaleOut": {
			perf.PodReady.P95.Experimental(71), perf.PodReady.P99.Experimental(72), perf.PodReady.Max.Experimental(72), perf.Nodes.Experimental(1250), perf.HeapMB.Experimental(507), perf.CPUCores.Experimental(1.769),
		},
		"topologyHostname/consolidation": {
			perf.TotalTimeSeconds.Experimental(1572), perf.Nodes.Experimental(840), perf.PodsDisrupted.Experimental(249), perf.HeapMB.Experimental(430), perf.CPUCores.Experimental(1.81),
		},
	},
	KWOKCI: perf.Levels{
		"topologyHostname/scaleOut": {
			perf.CPUCores.Experimental(1.168), perf.HeapMB.Experimental(296),
		},
		"topologyHostname/consolidation": {
			perf.CPUCores.Experimental(0.746), perf.HeapMB.Experimental(273),
		},
	},
}
