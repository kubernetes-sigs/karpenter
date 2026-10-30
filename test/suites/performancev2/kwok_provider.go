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
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/env"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

//go:embed calibration/kwok.json
var kwokCalibration []byte

// Provider returns the plugin named by KARPENTER_PERF_PROVIDER: kwok (the default) or kwok-ci.
func Provider(c env.Clients) *perf.Provider {
	if os.Getenv("KARPENTER_PERF_PROVIDER") == "kwok-ci" {
		return KWOKCI(c)
	}
	return KWOK(c)
}

// KWOK is the core KWOK provider's plugin: how to reach the controller, which NodeClass and
// NodePool to start from, how big each vector is, and the level of every phase it runs.
func KWOK(c env.Clients) *perf.Provider {
	return &perf.Provider{
		Name:       "kwok",
		Summary:    "Reference environment for releases and periodics on dedicated hardware. Levels are calibrated on it.",
		Client:     c.Client,
		KubeClient: c.KubeClient,
		Controller: perf.Controller{
			Namespace:   "kube-system",
			Selector:    labels.SelectorFromSet(map[string]string{"app.kubernetes.io/name": "karpenter"}),
			MetricsPort: "8080",
		},
		NodeClass: func() client.Object { return env.KWOKNodeClass() },
		NodePool: func(nc client.Object) *v1.NodePool {
			np := env.NodePool(nc)
			test.ReplaceRequirements(np, v1.NodeSelectorRequirementWithMinValues{
				Key: v1alpha1.InstanceSizeLabelKey, Operator: corev1.NodeSelectorOpLt, Values: []string{"32"},
			})
			np.Spec.Limits = v1.Limits{}
			np.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
			np.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("30s")
			np.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			return np
		},
		// Bounds follow calibration/kwok.json (make perf-calibrate): 1.1x the baseline max (0.9x the min for
		// utilization), pod_ready rounded up to whole seconds. Stable only where make perf-inventory's evidence
		// check passes.
		Calibration: perf.MustLoadCalibration(kwokCalibration),
		Levels: perf.Levels{
			"pods/scaleOut": {
				perf.PodReady.P95.Stable(16), perf.PodReady.P99.Stable(18), perf.PodReady.Max.Experimental(19),
				perf.CPUUtil.Experimental(0.759), perf.MemoryUtil.Experimental(0.713), perf.Nodes.Experimental(182),
				perf.HeapMB.Stable(75.5), perf.CPUCores.Experimental(0.87),
			},
			"pods/consolidation": {
				perf.TotalTimeSeconds.Experimental(1102), perf.PodReady.P95.Experimental(3), perf.PodReady.P99.Experimental(4), perf.PodReady.Max.Experimental(4),
				perf.CPUUtil.Experimental(0.879), perf.MemoryUtil.Experimental(0.782), perf.Nodes.Experimental(140), perf.PodsDisrupted.Experimental(448),
				perf.HeapMB.Experimental(80.9), perf.CPUCores.Experimental(0.232),
			},
			"nodepools/scheduling": {
				perf.TotalTimeSeconds.Stable(5), perf.PodReady.P95.Experimental(5), perf.PodReady.Max.Experimental(5),
				perf.HeapMB.Experimental(101), perf.CPUCores.Experimental(0.88),
			},
			"nodepools/startup": {
				perf.StartupSeconds.Experimental(25.6), perf.HeapMB.Experimental(75.5),
			},
			"topology/scaleOut": {
				perf.PodReady.P95.Experimental(37), perf.PodReady.P99.Experimental(38), perf.PodReady.Max.Experimental(39),
				perf.Nodes.Experimental(777), perf.HeapMB.Experimental(237), perf.CPUCores.Experimental(1.15),
			},
		},
	}
}
