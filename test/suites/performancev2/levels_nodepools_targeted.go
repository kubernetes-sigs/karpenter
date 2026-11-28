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

// nodepoolsTargetedLevels: Experimental starting bounds; calibrate before promoting any to Stable.
var nodepoolsTargetedLevels = scenarioLevels{
	KWOK: perf.Levels{
		"nodepoolsTargeted/scaleOut": {
			perf.PodReady.P95.Experimental(14), perf.PodReady.P99.Experimental(14), perf.PodReady.Max.Experimental(14), perf.Nodes.Experimental(180), perf.HeapMB.Experimental(190), perf.CPUCores.Experimental(1.548),
		},
		"nodepoolsTargeted/consolidation": {
			perf.TotalTimeSeconds.Experimental(354), perf.Nodes.Experimental(90), perf.HeapMB.Experimental(178), perf.CPUCores.Experimental(1.435),
		},
	},
	KWOKCI: perf.Levels{
		"nodepoolsTargeted/scaleOut":      {},
		"nodepoolsTargeted/consolidation": {},
	},
}
