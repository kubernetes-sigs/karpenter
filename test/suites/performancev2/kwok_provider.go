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
		NodeClass: func(*perf.Scenario) client.Object { return env.KWOKNodeClass() },
		NodePool: func(_ *perf.Scenario, nc client.Object) *v1.NodePool {
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
		// Bounds follow calibration/kwok.json (cmd/calibrate): the larger of 1.2x the baseline max and median +
		// 2.33 sd (the smaller of max/1.15 and median - 2.33 sd for utilization). Stable only where cmd/inventory's
		// evidence check passes. Heap baselines moved 24-33% between the October and November EKS runs, and October's
		// core reproduces the November numbers, so the environment moved, not core. Heap stays Experimental until it
		// holds across environments.
		Calibration: perf.MustLoadCalibration(kwokCalibration),
		Levels: perf.Levels{
			"pods/scaleOut": {
				perf.PodReady.P95.Stable(18), perf.PodReady.P99.Stable(18), perf.PodReady.Max.Experimental(21),
				perf.CPUUtil.Experimental(0.741), perf.MemoryUtil.Experimental(0.689), perf.Nodes.Experimental(200),
				perf.HeapMB.Experimental(102.2), perf.CPUCores.Experimental(0.785),
			},
			"pods/consolidation": {
				perf.TotalTimeSeconds.Experimental(1444), perf.CPUUtil.Experimental(0.819), perf.MemoryUtil.Experimental(0.722),
				perf.Nodes.Experimental(170), perf.PodsDisrupted.Experimental(525), perf.HeapMB.Experimental(115.9), perf.CPUCores.Experimental(0.313),
			},
			"nodepools/scheduling": {
				perf.TotalTimeSeconds.Stable(5), perf.PodReady.P95.Experimental(5), perf.HeapMB.Experimental(122.8), perf.CPUCores.Experimental(0.899),
			},
			"nodepools/startup": {
				perf.StartupSeconds.Experimental(5), perf.HeapMB.Experimental(74.6),
			},
			"topology/scaleOut": {
				perf.PodReady.P95.Experimental(35), perf.PodReady.P99.Experimental(36), perf.PodReady.Max.Experimental(41),
				perf.Nodes.Experimental(874), perf.HeapMB.Experimental(316), perf.CPUCores.Experimental(1.265),
			},
		},
	}
}
