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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Ranking", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})

	Context("Two-tier partitioning", func() {
		It("should sort normal nodes before do-not-disrupt nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// Put a do-not-disrupt pod on node 1
			dndPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"},
				},
				NodeName: nodes[1].Name,
			})
			// Normal pods on nodes 0 and 2
			pod0 := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
			pod2 := test.Pod(test.PodOptions{NodeName: nodes[2].Name})
			ExpectApplied(ctx, env.Client, dndPod, pod0, pod2)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			engine := deletioncost.NewRankingEngine()
			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := engine.RankNodes(ctx, env.Client, stateNodes)
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(3))

			// All non-DND nodes should have lower ranks than DND nodes
			var normalRanks, dndRanks []int
			for _, r := range ranks {
				if r.HasDoNotDisrupt {
					dndRanks = append(dndRanks, r.Rank)
				} else {
					normalRanks = append(normalRanks, r.Rank)
				}
			}
			Expect(normalRanks).To(HaveLen(2))
			Expect(dndRanks).To(HaveLen(1))
			for _, nr := range normalRanks {
				for _, dr := range dndRanks {
					Expect(nr).To(BeNumerically("<", dr))
				}
			}
		})

		It("should handle empty node list", func() {
			engine := deletioncost.NewRankingEngine()
			ranks, err := engine.RankNodes(ctx, env.Client, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(BeEmpty())
		})

		It("should handle all nodes being normal (no DND)", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			for _, n := range nodes {
				ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: n.Name}))
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			engine := deletioncost.NewRankingEngine()
			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := engine.RankNodes(ctx, env.Client, stateNodes)
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(3))
			for _, r := range ranks {
				Expect(r.HasDoNotDisrupt).To(BeFalse())
			}
		})

		It("should handle all nodes having do-not-disrupt pods", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			for _, n := range nodes {
				ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
					NodeName:   n.Name,
				}))
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			engine := deletioncost.NewRankingEngine()
			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := engine.RankNodes(ctx, env.Client, stateNodes)
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(2))
			for _, r := range ranks {
				Expect(r.HasDoNotDisrupt).To(BeTrue())
			}
		})

		It("should assign sequential ranks starting from -len(nodes)", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			for _, n := range nodes {
				ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: n.Name}))
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			engine := deletioncost.NewRankingEngine()
			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := engine.RankNodes(ctx, env.Client, stateNodes)
			Expect(err).ToNot(HaveOccurred())

			// Ranks should be sequential starting from -len(nodes)
			baseRank := -len(stateNodes)
			rankValues := make(map[int]bool)
			for _, r := range ranks {
				rankValues[r.Rank] = true
			}
			for i := 0; i < len(ranks); i++ {
				Expect(rankValues).To(HaveKey(baseRank + i))
			}
		})

		It("should produce mixed results with some normal and some DND nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(4, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// Nodes 0,2 normal; nodes 1,3 DND
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[0].Name}))
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
				NodeName:   nodes[1].Name,
			}))
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[2].Name}))
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
				NodeName:   nodes[3].Name,
			}))
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			engine := deletioncost.NewRankingEngine()
			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := engine.RankNodes(ctx, env.Client, stateNodes)
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(4))

			var normalCount, dndCount int
			baseRank := -len(stateNodes)
			maxNormalRank := baseRank - 1
			minDNDRank := baseRank + 100
			for _, r := range ranks {
				if r.HasDoNotDisrupt {
					dndCount++
					if r.Rank < minDNDRank {
						minDNDRank = r.Rank
					}
				} else {
					normalCount++
					if r.Rank > maxNormalRank {
						maxNormalRank = r.Rank
					}
				}
			}
			Expect(normalCount).To(Equal(2))
			Expect(dndCount).To(Equal(2))
			Expect(maxNormalRank).To(BeNumerically("<", minDNDRank))
		})
	})
})
