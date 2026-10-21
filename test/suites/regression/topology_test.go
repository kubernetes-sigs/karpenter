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

package integration_test

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

var _ = Describe("TopologySpread", func() {
	// Issue #2623: https://github.com/kubernetes-sigs/karpenter/issues/2623
	// Topology spread constraints must only count domains from NodePools compatible with the pod. Here the pods can
	// only schedule to a single-zone NodePool selected via a custom label and toleration, while the default NodePool
	// produces every other zone. Without domain filtering the unreachable zones pin the topology's global minimum at
	// zero and only maxSkew pods ever schedule; with it, only the compatible NodePool's zone is counted and the
	// whole deployment schedules there.
	It("should only count topology domains from NodePools compatible with the pod (Issue #2623)", func() {
		if !env.IsDefaultNodeClassKWOK() {
			Skip("the zone fixtures in this test assume the KWOK provider's zones")
		}

		// NodePool the pods select: tainted, custom label, restricted to the KWOK provider's test-zone-a only.
		isolatedNodePool := env.DefaultNodePool(nodeClass)
		isolatedNodePool.Spec.Template.Labels = lo.Assign(isolatedNodePool.Spec.Template.Labels, map[string]string{
			"test-topology/pool": "isolated",
		})
		isolatedNodePool.Spec.Template.Spec.Taints = []corev1.Taint{{
			Key:    "test-topology/dedicated",
			Value:  "isolated",
			Effect: corev1.TaintEffectNoSchedule,
		}}
		test.ReplaceRequirements(isolatedNodePool,
			v1.NodeSelectorRequirementWithMinValues{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-a"},
			},
			v1.NodeSelectorRequirementWithMinValues{
				Key:      v1alpha1.InstanceSizeLabelKey,
				Operator: corev1.NodeSelectorOpLt,
				Values:   []string{"32"},
			},
		)

		podLabels := map[string]string{"app": "isolated-app"}
		numPods := 3
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: int32(numPods),
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				NodeSelector: map[string]string{
					"test-topology/pool": "isolated",
				},
				Tolerations: []corev1.Toleration{{
					Key:      "test-topology/dedicated",
					Value:    "isolated",
					Effect:   corev1.TaintEffectNoSchedule,
					Operator: corev1.TolerationOpEqual,
				}},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					TopologyKey:        corev1.LabelTopologyZone,
					WhenUnsatisfiable:  corev1.DoNotSchedule,
					MaxSkew:            1,
					LabelSelector:      &metav1.LabelSelector{MatchLabels: podLabels},
					NodeTaintsPolicy:   lo.ToPtr(corev1.NodeInclusionPolicyHonor),
					NodeAffinityPolicy: lo.ToPtr(corev1.NodeInclusionPolicyHonor),
				}},
			},
		})

		// The default NodePool produces every zone the isolated NodePool cannot reach.
		env.ExpectCreated(nodeClass, nodePool, isolatedNodePool, dep)

		selector := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
		pods := env.EventuallyExpectHealthyPodCount(selector, numPods)
		for _, pod := range pods {
			node := &corev1.Node{}
			Expect(env.Client.Get(env, client.ObjectKey{Name: pod.Spec.NodeName}, node)).To(Succeed())
			Expect(node.Labels[corev1.LabelTopologyZone]).To(Equal("test-zone-a"))
			Expect(node.Labels[v1.NodePoolLabelKey]).To(Equal(isolatedNodePool.Name))
		}
	})
})
