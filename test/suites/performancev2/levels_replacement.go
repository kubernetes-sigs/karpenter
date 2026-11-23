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

// replacementLevels: see levels.go for the rule. Utilization is measured only until it's recalibrated without
// DaemonSet requests; replacementDoNotDisrupt and per-node replacement latency are measured only until calibrated.
// unavailable_pod_seconds < 44 is 1.2x the max of 3 baselines (17.3, 36.1, 24.1 on 150cb10e); mX-drift-noprespin
// (drain before the replacement initializes) reads 543.5.
var replacementLevels = scenarioLevels{
	KWOK: perf.Levels{
		"replacement/initial": {
			perf.PodReady.P95.Stable(20), perf.PodReady.P99.Stable(21), perf.PodReady.Max.Experimental(21), perf.Nodes.Experimental(360), perf.HeapMB.Experimental(213), perf.CPUCores.Experimental(1.154),
		},
		"replacement/drift": {
			perf.TotalTimeSeconds.Experimental(512), perf.UnavailablePodSeconds.Experimental(44), perf.Nodes.Experimental(360), perf.PodsDisrupted.Experimental(720), perf.HeapMB.Experimental(249), perf.CPUCores.Experimental(1.019),
		},
	},
	KWOKCI: perf.Levels{
		"replacement/initial": {
			perf.CPUCores.Experimental(0.722), perf.HeapMB.Experimental(176), perf.Nodes.Experimental(409),
		},
		"replacement/drift": {
			perf.CPUCores.Experimental(0.908), perf.HeapMB.Experimental(221), perf.Nodes.Experimental(362),
		},
	},
}
