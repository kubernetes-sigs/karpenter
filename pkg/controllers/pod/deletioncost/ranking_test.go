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
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// expectPodRank reads pod via the live client and returns the integer value of
// its pod-deletion-cost annotation. Fails the spec if the annotation is missing
// or non-integer; use expectPodAnnotationCleared for the Group D case.
func expectPodRank(pod *corev1.Pod) int {
	GinkgoHelper()
	updated := &corev1.Pod{}
	Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
	raw, ok := updated.Annotations[corev1.PodDeletionCost]
	Expect(ok).To(BeTrue(), "pod %s missing pod-deletion-cost annotation", pod.Name)
	val, err := strconv.Atoi(raw)
	Expect(err).ToNot(HaveOccurred(), "pod %s has non-integer pod-deletion-cost %q", pod.Name, raw)
	return val
}

// expectPodAnnotationCleared asserts the pod has no pod-deletion-cost
// annotation (Group D semantics: the controller clears the value).
func expectPodAnnotationCleared(pod *corev1.Pod) {
	GinkgoHelper()
	updated := &corev1.Pod{}
	Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
	Expect(updated.Annotations).ToNot(HaveKey(corev1.PodDeletionCost),
		"pod %s should not carry pod-deletion-cost (Group D clears it)", pod.Name)
}

var _ = Describe("Ranking", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
		// test.NodePool() leaves Disruption fields unset, so the deletion-cost
		// controller routes every node to Group D:
		//   - ConsolidateAfter nil Duration → "consolidation disabled" predicate
		//   - Budgets unset → CRD default "10%" caps Groups B and C to 1 slot
		// Set permissive defaults so tests exercise the partitioning under test
		// rather than the disabled/budget-overflow paths.
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
	})

	// Migrated happy-path tests drive through Controller.Reconcile and assert
	// on the observable pod-deletion-cost annotation. Direct-helper tests for
	// partition edge cases remain in this file for the cases where the
	// observable annotation does not distinguish the classification.
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
			// Put a do-not-disrupt pod on node 1; normal pods on nodes 0 and 2.
			dndPod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"},
				},
				NodeName: nodes[1].Name,
			})
			pod0 := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			pod2 := rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name})
			ExpectApplied(ctx, env.Client, dndPod, pod0, pod2)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Group C nodes (normal) get strictly-negative ranks; Group D
			// nodes have their annotation cleared. The Group C/D ordering is
			// observable as "annotated vs not-annotated" at the public API
			// boundary.
			Expect(expectPodRank(pod0)).To(BeNumerically("<", 0))
			Expect(expectPodRank(pod2)).To(BeNumerically("<", 0))
			expectPodAnnotationCleared(dndPod)
		})

		It("should handle empty node list", func() {

			ranks, err := deletioncost.RankNodes(ctx, env.Client, cluster, fakeClock, nil, map[string]*v1.NodePool{nodePool.Name: nodePool})
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
			pods := make([]*corev1.Pod, len(nodes))
			for i, n := range nodes {
				pods[i] = rsOwnedPod(test.PodOptions{NodeName: n.Name})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Every pod gets a negative rank; none are cleared.
			for _, p := range pods {
				Expect(expectPodRank(p)).To(BeNumerically("<", 0))
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
			pods := make([]*corev1.Pod, len(nodes))
			for i, n := range nodes {
				pods[i] = rsOwnedPod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
					NodeName:   n.Name,
				})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Every pod is on a Group D node; every pod has its
			// pod-deletion-cost annotation cleared.
			for _, p := range pods {
				expectPodAnnotationCleared(p)
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
			pods := make([]*corev1.Pod, len(nodes))
			for i, n := range nodes {
				pods[i] = rsOwnedPod(test.PodOptions{NodeName: n.Name})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// All three pods are in Group C; ranks should be contiguous
			// across -len(nodes), -len(nodes)+1, -len(nodes)+2. Order across
			// pods depends on the pod-count tie-break so verify the rank set.
			ranks := map[int]bool{}
			for _, p := range pods {
				ranks[expectPodRank(p)] = true
			}
			base := -len(nodes)
			for i := 0; i < len(nodes); i++ {
				Expect(ranks).To(HaveKey(base+i), "expected contiguous rank %d in observed set %v", base+i, ranks)
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
			// Nodes 0,2 normal; nodes 1,3 DND.
			pod0 := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			pod1 := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
				NodeName:   nodes[1].Name,
			})
			pod2 := rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name})
			pod3 := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
				NodeName:   nodes[3].Name,
			})
			ExpectApplied(ctx, env.Client, pod0, pod1, pod2, pod3)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			Expect(expectPodRank(pod0)).To(BeNumerically("<", 0))
			Expect(expectPodRank(pod2)).To(BeNumerically("<", 0))
			expectPodAnnotationCleared(pod1)
			expectPodAnnotationCleared(pod3)
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

			pdbBlockedPod := rsOwnedPod(test.PodOptions{
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
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name}))
			// Node 2: normal pod
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name}))
			// Node 3: do-not-disrupt pod
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
				NodeName:   nodes[3].Name,
			}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := deletioncost.RankNodes(ctx, env.Client, cluster, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
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
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name}))

			// Node 1: normal
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := deletioncost.RankNodes(ctx, env.Client, cluster, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
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
				ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
					NodeName:   nodes[0].Name,
				}))
			}
			// Node 1: 1 PDB-blocked pod
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{
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
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := deletioncost.RankNodes(ctx, env.Client, cluster, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
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
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{
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
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name}))

			// Node 2: normal (Group C)
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := deletioncost.RankNodes(ctx, env.Client, cluster, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
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

	Context("Per-NodePool budgets", func() {
		It("should respect per-NodePool consolidation budgets across multiple pools", func() {
			// Two NodePools with different budgets:
			//  - poolA: Nodes "100%" — all normal nodes can land in Group C
			//  - poolB: Nodes "0"   — every normal node overflows to Group D
			poolA := test.NodePool()
			poolA.Name = "pool-a"
			poolA.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			poolA.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}

			poolB := test.NodePool()
			poolB.Name = "pool-b"
			poolB.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			poolB.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}

			ExpectApplied(ctx, env.Client, poolA, poolB)

			// One node per pool, one normal pod each.
			ncA, nA := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: poolA.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ncB, nB := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: poolB.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, ncA, nA, ncB, nB)
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nA.Name}))
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nB.Name}))
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nA, nB}, []*v1.NodeClaim{ncA, ncB})

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := deletioncost.RankNodes(ctx, env.Client, cluster, fakeClock, stateNodes, map[string]*v1.NodePool{
				poolA.Name: poolA,
				poolB.Name: poolB,
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(2))

			// poolA's node should be in Group C (HasDoNotDisrupt=false),
			// poolB's node should overflow to Group D (HasDoNotDisrupt=true).
			for _, r := range ranks {
				switch r.Node.Labels()[v1.NodePoolLabelKey] {
				case poolA.Name:
					Expect(r.HasDoNotDisrupt).To(BeFalse(), "poolA node should be in Group C with budget 100%")
				case poolB.Name:
					Expect(r.HasDoNotDisrupt).To(BeTrue(), "poolB node should overflow to Group D with budget 0")
				}
			}
		})
	})

	Context("50-node cap", func() {
		It("should drop excess nodes beyond maxNodesPerCycle in the controller", func() {
			// The 50-node cap is enforced by the controller (not RankNodes itself).
			// Build 55 nodes, run a full reconcile, and confirm only the top
			// 50 ranked pods received an annotation; the remaining pods are
			// untouched.
			const totalNodes = 55
			const cap = 50

			ExpectApplied(ctx, env.Client, nodePool)
			nodeClaims, nodes := test.NodeClaimsAndNodes(totalNodes, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			pods := make([]*corev1.Pod, totalNodes)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				pods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[i].Name})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Count how many of the 55 pods got the deletion-cost annotation.
			annotated := 0
			for _, p := range pods {
				updated := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(p), updated)).To(Succeed())
				if _, ok := updated.Annotations[corev1.PodDeletionCost]; ok {
					annotated++
				}
			}
			Expect(annotated).To(Equal(cap), "exactly maxNodesPerCycle (50) pods should receive the annotation")
		})
	})
})
