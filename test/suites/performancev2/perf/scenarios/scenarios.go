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

package scenarios

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// All lists every scenario, grouped: each customer question first, then its variants (Scenario.Group).
var All = []perf.Scenario{
	Pods, PodsWide, PodsBalanced, PodsBalancedChurn, PodsBalancedHeterogeneous,
	NodePools,
	Topology,
	Replacement,
	ReplacementDoNotDisrupt,
}

func zoneSpread() test.DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		labels := opts.PodOptions.Labels
		if labels == nil {
			labels = map[string]string{"app": opts.Name}
		}
		opts.PodOptions.TopologySpreadConstraints = append(opts.PodOptions.TopologySpreadConstraints, corev1.TopologySpreadConstraint{
			MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
		})
	}
}
