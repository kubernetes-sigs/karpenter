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
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

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

	Context("Group A: Disrupted + PDB-blocked nodes", func() {
		It("should rank disrupted+PDB-blocked nodes below all other groups", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(4, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Node 0: disrupted (has taint) + PDB-blocked pod
			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodes[0])

			pdbBlockedPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "blocked"},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pdbBlockedPod)

			// Create a PDB that blocks all disruptions for the pod
			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "block-all",
					Namespace: pdbBlockedPod.Namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 0,
				},
			}
			ExpectApplied(ctx, env.Client, pdb)

			// Node 1: normal pod
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[1].Name}))
			// Node 2: normal pod
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[2].Name}))
			// Node 3: do-not-disrupt pod
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

			// Find the rank for node 0 (disrupted+blocked) - should be math.MinInt32
			for _, r := range ranks {
				if r.Node.Node.Name == nodes[0].Name {
					Expect(r.Rank).To(Equal(math.MinInt32))
				} else {
					Expect(r.Rank).To(BeNumerically(">", math.MinInt32))
				}
			}
		})

		It("should not classify disrupted node without PDB-blocked pods as Group A", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Node 0: disrupted taint but no PDB-blocked pods
			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodes[0])
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[0].Name}))

			// Node 1: normal
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[1].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			engine := deletioncost.NewRankingEngine()
			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := engine.RankNodes(ctx, env.Client, stateNodes)
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(2))

			// Both should be in the normal tier (no Group A) since there's no PDB blocking
			for _, r := range ranks {
				Expect(r.HasDoNotDisrupt).To(BeFalse())
			}
		})

		It("should rank multiple disrupted+PDB-blocked nodes by pod count ascending", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Both node 0 and node 1 are disrupted + PDB-blocked
			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			nodes[1].Spec.Taints = append(nodes[1].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodes[0], nodes[1])

			// Node 0: 3 PDB-blocked pods
			for i := 0; i < 3; i++ {
				ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
					NodeName:   nodes[0].Name,
				}))
			}
			// Node 1: 1 PDB-blocked pod
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
				NodeName:   nodes[1].Name,
			}))

			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "block-all",
					Namespace: "default",
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 0,
				},
			}
			ExpectApplied(ctx, env.Client, pdb)

			// Node 2: normal
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[2].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			engine := deletioncost.NewRankingEngine()
			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := engine.RankNodes(ctx, env.Client, stateNodes)
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(3))

			// Find ranks for disrupted+blocked nodes
			var node0Rank, node1Rank, node2Rank int
			for _, r := range ranks {
				switch r.Node.Node.Name {
				case nodes[0].Name:
					node0Rank = r.Rank
				case nodes[1].Name:
					node1Rank = r.Rank
				case nodes[2].Name:
					node2Rank = r.Rank
				}
			}
			// Both disrupted+blocked nodes should get math.MinInt32
			Expect(node0Rank).To(Equal(math.MinInt32))
			Expect(node1Rank).To(Equal(math.MinInt32))
			// Normal node should have a rank greater than math.MinInt32
			Expect(node2Rank).To(BeNumerically(">", math.MinInt32))
		})

		It("should place Group A below Group B (drifted) in ordering", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Node 0: disrupted + PDB-blocked (Group A)
			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodes[0])
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
				NodeName:   nodes[0].Name,
			}))
			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "block-all",
					Namespace: "default",
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 0,
				},
			}
			ExpectApplied(ctx, env.Client, pdb)

			// Node 1: drifted (Group B)
			nodeClaims[1].StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			ExpectApplied(ctx, env.Client, nodeClaims[1])
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[1].Name}))

			// Node 2: normal (Group C)
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[2].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			engine := deletioncost.NewRankingEngine()
			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := engine.RankNodes(ctx, env.Client, stateNodes)
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(3))

			var groupARank, groupBRank, groupCRank int
			for _, r := range ranks {
				switch r.Node.Node.Name {
				case nodes[0].Name:
					groupARank = r.Rank
				case nodes[1].Name:
					groupBRank = r.Rank
				case nodes[2].Name:
					groupCRank = r.Rank
				}
			}
			// Group A gets math.MinInt32, Group B < Group C
			Expect(groupARank).To(Equal(math.MinInt32))
			Expect(groupBRank).To(BeNumerically("<", groupCRank))
		})
	})
})
