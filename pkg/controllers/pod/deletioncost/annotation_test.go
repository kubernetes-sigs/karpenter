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

	Context("Sentinel annotation detection", func() {
		It("should add both deletion cost and sentinel annotations to unmanaged pods", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: -10, HasDoNotDisrupt: false}}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			// Verify pod has both annotations
			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).To(HaveKeyWithValue(deletioncost.PodDeletionCostAnnotation, "-10"))
			Expect(updatedPod.Annotations).To(HaveKey(deletioncost.KarpenterManagedDeletionCostAnnotation))
		})

		It("should skip pods with customer-managed deletion cost (no sentinel)", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// Pod with customer-set deletion cost but no sentinel
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						deletioncost.PodDeletionCostAnnotation: "100",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: -10, HasDoNotDisrupt: false}}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			// Verify pod still has original customer value
			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[deletioncost.PodDeletionCostAnnotation]).To(Equal("100"))
			Expect(updatedPod.Annotations).ToNot(HaveKey(deletioncost.KarpenterManagedDeletionCostAnnotation))
		})

		It("should update pods with sentinel annotation (Karpenter-managed)", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// Pod already managed by Karpenter (has both annotations)
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						deletioncost.PodDeletionCostAnnotation:              "-5",
						deletioncost.KarpenterManagedDeletionCostAnnotation: "true",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: -20, HasDoNotDisrupt: false}}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			// Verify pod was updated to new rank
			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[deletioncost.PodDeletionCostAnnotation]).To(Equal("-20"))
			Expect(updatedPod.Annotations).To(HaveKey(deletioncost.KarpenterManagedDeletionCostAnnotation))
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
			pod := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: -3, HasDoNotDisrupt: false}}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[deletioncost.PodDeletionCostAnnotation]).To(Equal("-3"))
			Expect(updatedPod.Annotations[deletioncost.KarpenterManagedDeletionCostAnnotation]).To(Equal("true"))
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
				pods[i] = test.Pod(test.PodOptions{NodeName: nodes[0].Name})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: -7, HasDoNotDisrupt: false}}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			for _, pod := range pods {
				updatedPod := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
				Expect(updatedPod.Annotations[deletioncost.PodDeletionCostAnnotation]).To(Equal("-7"))
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
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: -1, HasDoNotDisrupt: false}}
			// Should not error even with no pods
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())
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
			pod0 := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
			pod1 := test.Pod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, pod0, pod1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}
			Expect(stateNodes).To(HaveLen(2))

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{
				{Node: stateNodes[0], Rank: -10, HasDoNotDisrupt: false},
				{Node: stateNodes[1], Rank: -9, HasDoNotDisrupt: false},
			}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

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
					Expect(updatedPod.Annotations[deletioncost.PodDeletionCostAnnotation]).To(Equal(expectedRank))
				}
			}
		})
	})

	Context("Group D (do-not-disrupt) annotation clearing", func() {
		It("should clear managed annotations on do-not-disrupt nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// Pod previously managed by Karpenter (has both annotations)
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						deletioncost.PodDeletionCostAnnotation:              "5",
						deletioncost.KarpenterManagedDeletionCostAnnotation: "true",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: 10, HasDoNotDisrupt: true}}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			// Both managed annotations should be removed
			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).ToNot(HaveKey(deletioncost.PodDeletionCostAnnotation))
			Expect(updatedPod.Annotations).ToNot(HaveKey(deletioncost.KarpenterManagedDeletionCostAnnotation))
		})

		It("should not modify unmanaged pods on do-not-disrupt nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// Pod with customer-set cost but no sentinel
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						deletioncost.PodDeletionCostAnnotation: "100",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: 10, HasDoNotDisrupt: true}}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			// Customer annotation should be preserved
			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[deletioncost.PodDeletionCostAnnotation]).To(Equal("100"))
			Expect(updatedPod.Annotations).ToNot(HaveKey(deletioncost.KarpenterManagedDeletionCostAnnotation))
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
			pod := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: 10, HasDoNotDisrupt: true}}
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			// Pod should remain unchanged (no annotations added)
			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).ToNot(HaveKey(deletioncost.PodDeletionCostAnnotation))
			Expect(updatedPod.Annotations).ToNot(HaveKey(deletioncost.KarpenterManagedDeletionCostAnnotation))
		})
	})

	Context("Third-party conflict detection", func() {
		It("should detect externally modified annotation and remove sentinel", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			mgr := deletioncost.NewAnnotationManager(env.Client, recorder)
			nodeRanks := []deletioncost.NodeRank{{Node: stateNodes[0], Rank: -5, HasDoNotDisrupt: false}}

			// First update — Karpenter sets the annotation
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())
			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[deletioncost.PodDeletionCostAnnotation]).To(Equal("-5"))

			// Simulate third-party modifying the annotation
			updatedPod.Annotations[deletioncost.PodDeletionCostAnnotation] = "999"
			Expect(env.Client.Update(ctx, updatedPod)).To(Succeed())

			// Second update — should detect conflict, remove sentinel, skip pod
			Expect(mgr.UpdatePodDeletionCosts(ctx, nodeRanks)).To(Succeed())

			finalPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), finalPod)).To(Succeed())
			// Sentinel should be removed
			Expect(finalPod.Annotations).ToNot(HaveKey(deletioncost.KarpenterManagedDeletionCostAnnotation))
			// Third-party value should be preserved
			Expect(finalPod.Annotations[deletioncost.PodDeletionCostAnnotation]).To(Equal("999"))
		})
	})
})
