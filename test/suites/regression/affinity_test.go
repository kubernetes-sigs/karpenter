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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

func nodeAffinityRequirement(key string, operator corev1.NodeSelectorOperator) corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{
		Key:      key,
		Operator: operator,
	}
}

func requiredNodeAffinity(terms ...corev1.NodeSelectorTerm) *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms},
	}}
}

func preferredNodeAffinity(requirements ...corev1.NodeSelectorRequirement) *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: requirements,
			},
		}},
	}}
}

var _ = Describe("Node Affinity Classification", func() {
	DescribeTable("should schedule pods that can run on Karpenter nodes", func(configure func(*corev1.Pod)) {
		pod := test.Pod()
		configure(pod)

		env.ExpectCreated(nodeClass, nodePool, pod)
		env.EventuallyExpectHealthy(pod)

		node := &corev1.Node{}
		Expect(env.Client.Get(env, client.ObjectKey{Name: pod.Spec.NodeName}, node)).To(Succeed())
		Expect(node.Labels).To(HaveKeyWithValue(v1.NodePoolLabelKey, nodePool.Name))
	},
		Entry("with a preferred DoesNotExist affinity", func(pod *corev1.Pod) {
			pod.Spec.Affinity = preferredNodeAffinity(
				nodeAffinityRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpDoesNotExist),
			)
		}),
		Entry("with a NodePool nodeSelector and preferred DoesNotExist affinity", func(pod *corev1.Pod) {
			pod.Spec.NodeSelector = map[string]string{v1.NodePoolLabelKey: nodePool.Name}
			pod.Spec.Affinity = preferredNodeAffinity(
				nodeAffinityRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpDoesNotExist),
			)
		}),
		Entry("when an excluding required OR term comes first", func(pod *corev1.Pod) {
			pod.Spec.Affinity = requiredNodeAffinity(
				corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{
					nodeAffinityRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpDoesNotExist),
				}},
				corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{
					nodeAffinityRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpExists),
				}},
			)
		}),
		Entry("when an excluding required OR term comes last", func(pod *corev1.Pod) {
			pod.Spec.Affinity = requiredNodeAffinity(
				corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{
					nodeAffinityRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpExists),
				}},
				corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{
					nodeAffinityRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpDoesNotExist),
				}},
			)
		}),
		Entry("when required affinity permits Karpenter despite a preferred exclusion", func(pod *corev1.Pod) {
			pod.Spec.Affinity = requiredNodeAffinity(corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					nodeAffinityRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpExists),
				},
			})
			pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = preferredNodeAffinity(
				nodeAffinityRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpDoesNotExist),
			).NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		}),
	)

	DescribeTable("should preserve hard exclusions from Karpenter nodes", func(affinity *corev1.Affinity) {
		pod := test.Pod()
		pod.Spec.Affinity = affinity
		env.ExpectCreated(nodeClass, nodePool, pod)

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(pod), &corev1.Pod{})).To(Succeed())
		}).Should(Succeed())
		Consistently(func(g Gomega) {
			current := &corev1.Pod{}
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(pod), current)).To(Succeed())
			g.Expect(current.Spec.NodeName).To(BeEmpty())

			nodeClaims := &v1.NodeClaimList{}
			g.Expect(env.Client.List(env, nodeClaims, client.HasLabels{test.DiscoveryLabel})).To(Succeed())
			g.Expect(nodeClaims.Items).To(BeEmpty())
		}, 20*time.Second, 2*time.Second).Should(Succeed())

		events := &corev1.EventList{}
		Expect(env.Client.List(env, events, client.InNamespace(pod.Namespace))).To(Succeed())
		for _, event := range events.Items {
			if event.InvolvedObject.Name == pod.Name {
				Expect(event.Source.Component).ToNot(Equal("karpenter"))
			}
		}
	},
		Entry("with one required DoesNotExist term", requiredNodeAffinity(
			corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{
				nodeAffinityRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpDoesNotExist),
			}},
		)),
	)
})
