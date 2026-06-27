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

package deletioncost_test

import (
	"fmt"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Annotation", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})

	Context("Pod-deletion-cost write path", func() {
		It("should add the pod-deletion-cost annotation to pods without it", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -10
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: rank, HasDoNotDisrupt: false}}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).To(HaveKeyWithValue(corev1.PodDeletionCost, strconv.Itoa(rank)))
		})

		It("should overwrite customer-set pod-deletion-cost values", func() {
			// v4 RFC: gate-ON state means the user is OK with Karpenter managing the
			// pod-deletion-cost annotation. There is no overwrite-protection.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						corev1.PodDeletionCost: "100",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -10
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: rank, HasDoNotDisrupt: false}}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
		})

		It("should update existing pod-deletion-cost values to the new rank", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						corev1.PodDeletionCost: "-5",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -20
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: rank, HasDoNotDisrupt: false}}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
		})

		It("should handle pods without any annotations", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -3
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: rank, HasDoNotDisrupt: false}}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
		})

		It("should update multiple pods on the same node with the same rank", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pods := make([]*corev1.Pod, 3)
			for i := range pods {
				pods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -7
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: rank, HasDoNotDisrupt: false}}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			for _, pod := range pods {
				updatedPod := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
				Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
			}
		})

		It("should handle nodes with no pods", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: -1, HasDoNotDisrupt: false}}
			// Should not error even with no pods
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())
		})

		It("should update pods across multiple ranked nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod0 := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			pod1 := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, pod0, pod1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}
			Expect(stateNodes).To(HaveLen(2))

			nodeRanks := []deletioncost.NodeRank{
				{Node: stateNodes[0], Rank: -10, HasDoNotDisrupt: false},
				{Node: stateNodes[1], Rank: -9, HasDoNotDisrupt: false},
			}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			// Both pods should have their respective node's rank
			for _, sn := range stateNodes {
				var expectedRank string
				for _, nr := range nodeRanks {
					if nr.Node.Node.Name == sn.Node.Name {
						expectedRank = fmt.Sprintf("%d", nr.Rank)
					}
				}
				pods, err := sn.Pods(ctx, env.Client)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range pods {
					updatedPod := &corev1.Pod{}
					Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(p), updatedPod)).To(Succeed())
					Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(expectedRank))
				}
			}
		})
	})

	Context("Group D (do-not-disrupt) annotation clearing", func() {
		It("should clear the pod-deletion-cost annotation on do-not-disrupt nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						corev1.PodDeletionCost: "5",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: 10, HasDoNotDisrupt: true}}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
		})

		It("should skip pods without annotations on do-not-disrupt nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: 10, HasDoNotDisrupt: true}}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
		})
	})
})
