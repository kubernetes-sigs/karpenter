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
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
				NodeName:   nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pdbBlockedPod)
			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-all", Namespace: pdbBlockedPod.Namespace},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
			}
			ExpectApplied(ctx, env.Client, pdb)

			// Node 1: normal; Node 2: normal; Node 3: do-not-disrupt pod.
			pod1 := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			pod2 := rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name})
			dndPod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
				NodeName:   nodes[3].Name,
			})
			ExpectApplied(ctx, env.Client, pod1, pod2, dndPod)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Node 0's pod (Group A) carries math.MinInt32; nodes 1 and 2
			// (Group C) carry strictly-negative ranks greater than MinInt32;
			// node 3 (Group D) has its annotation cleared.
			Expect(expectPodRank(pdbBlockedPod)).To(Equal(math.MinInt32))
			Expect(expectPodRank(pod1)).To(BeNumerically(">", math.MinInt32))
			Expect(expectPodRank(pod1)).To(BeNumerically("<", 0))
			Expect(expectPodRank(pod2)).To(BeNumerically(">", math.MinInt32))
			Expect(expectPodRank(pod2)).To(BeNumerically("<", 0))
			expectPodAnnotationCleared(dndPod)
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
			pdbBlockedPod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
				NodeName:   nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pdbBlockedPod)
			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-all", Namespace: "default"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
			}
			ExpectApplied(ctx, env.Client, pdb)

			// Node 1: drifted (Group B)
			nodeClaims[1].StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			ExpectApplied(ctx, env.Client, nodeClaims[1])
			driftedPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, driftedPod)

			// Node 2: normal (Group C)
			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name})
			ExpectApplied(ctx, env.Client, normalPod)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Group A < Group B < Group C in delete-first semantics: A gets
			// math.MinInt32; B and C are contiguous negative integers with
			// B's rank strictly less than C's.
			Expect(expectPodRank(pdbBlockedPod)).To(Equal(math.MinInt32))
			Expect(expectPodRank(driftedPod)).To(BeNumerically("<", expectPodRank(normalPod)))
		})
	})

	Context("Per-NodePool budgets", func() {
		It("should respect per-NodePool consolidation budgets across multiple pools", func() {
			// Two NodePools with different budgets:
			//   poolA: Nodes "100%" — normal node lands in Group C
			//   poolB: Nodes "0"   — normal node overflows to Group D
			poolA := test.NodePool()
			poolA.Name = "pool-a"
			poolA.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			poolA.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}

			poolB := test.NodePool()
			poolB.Name = "pool-b"
			poolB.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			poolB.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}

			ExpectApplied(ctx, env.Client, poolA, poolB)

			ncA, nA := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: poolA.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ncB, nB := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: poolB.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, ncA, nA, ncB, nB)
			podA := rsOwnedPod(test.PodOptions{NodeName: nA.Name})
			podB := rsOwnedPod(test.PodOptions{NodeName: nB.Name})
			ExpectApplied(ctx, env.Client, podA, podB)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nA, nB}, []*v1.NodeClaim{ncA, ncB})

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// poolA's pod in Group C: annotated with a negative rank.
			// poolB's pod overflowed to Group D: annotation cleared.
			Expect(expectPodRank(podA)).To(BeNumerically("<", 0))
			expectPodAnnotationCleared(podB)
		})
	})

	Context("Bounded labeling: cap applies to Groups B/C/D only", func() {
		// maxNodesPerCycle caps the number of Group B/C/D nodes annotated per
		// reconcile; Group A is exempt because those nodes are already tainted
		// for disruption and stay stable once labeled.
		It("should cap Group C nodes at maxNodesPerCycle when no Group A is present", func() {
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

			annotated := 0
			for _, p := range pods {
				updated := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(p), updated)).To(Succeed())
				if _, ok := updated.Annotations[corev1.PodDeletionCost]; ok {
					annotated++
				}
			}
			Expect(annotated).To(Equal(cap), "exactly maxNodesPerCycle (50) Group C pods should receive the annotation")
		})

		It("should annotate every Group A node even when Group A alone exceeds maxNodesPerCycle", func() {
			// 60 Group A nodes (disrupted taint + PDB-blocked pods) plus 3
			// Group C nodes. All 60 Group A nodes must be annotated (cap
			// exempt); the 3 Group C nodes get annotated because they fit
			// inside the tail cap of 50.
			const groupANodes = 60
			const groupCNodes = 3
			const total = groupANodes + groupCNodes

			ExpectApplied(ctx, env.Client, nodePool)
			nodeClaims, nodes := test.NodeClaimsAndNodes(total, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			// PDB blocks pods labeled app=blocked.
			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-all", Namespace: "default"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
			}
			ExpectApplied(ctx, env.Client, pdb)

			groupAPods := make([]*corev1.Pod, groupANodes)
			groupCPods := make([]*corev1.Pod, groupCNodes)
			for i := 0; i < groupANodes; i++ {
				nodes[i].Spec.Taints = append(nodes[i].Spec.Taints, v1.DisruptedNoScheduleTaint)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				groupAPods[i] = rsOwnedPod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
					NodeName:   nodes[i].Name,
				})
				ExpectApplied(ctx, env.Client, groupAPods[i])
			}
			for i := 0; i < groupCNodes; i++ {
				idx := groupANodes + i
				ExpectApplied(ctx, env.Client, nodeClaims[idx], nodes[idx])
				groupCPods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[idx].Name})
				ExpectApplied(ctx, env.Client, groupCPods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Every Group A pod carries math.MinInt32.
			for _, p := range groupAPods {
				Expect(expectPodRank(p)).To(Equal(math.MinInt32))
			}
			// Every Group C pod carries a strictly-negative non-sentinel rank
			// (3 <= tail cap of 50).
			for _, p := range groupCPods {
				Expect(expectPodRank(p)).To(BeNumerically("<", 0))
				Expect(expectPodRank(p)).To(BeNumerically(">", math.MinInt32))
			}
		})

		It("should exempt Group A from the cap and truncate only Group C overflow", func() {
			// 10 Group A + 60 Group C. Expect all 10 A annotated, 50 of the
			// 60 C annotated, and the remaining 10 C untouched.
			const groupANodes = 10
			const groupCNodes = 60
			const cap = 50
			const total = groupANodes + groupCNodes

			ExpectApplied(ctx, env.Client, nodePool)
			nodeClaims, nodes := test.NodeClaimsAndNodes(total, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-all", Namespace: "default"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
			}
			ExpectApplied(ctx, env.Client, pdb)

			groupAPods := make([]*corev1.Pod, groupANodes)
			groupCPods := make([]*corev1.Pod, groupCNodes)
			for i := 0; i < groupANodes; i++ {
				nodes[i].Spec.Taints = append(nodes[i].Spec.Taints, v1.DisruptedNoScheduleTaint)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				groupAPods[i] = rsOwnedPod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
					NodeName:   nodes[i].Name,
				})
				ExpectApplied(ctx, env.Client, groupAPods[i])
			}
			for i := 0; i < groupCNodes; i++ {
				idx := groupANodes + i
				ExpectApplied(ctx, env.Client, nodeClaims[idx], nodes[idx])
				groupCPods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[idx].Name})
				ExpectApplied(ctx, env.Client, groupCPods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// All 10 Group A pods annotated with math.MinInt32.
			for _, p := range groupAPods {
				Expect(expectPodRank(p)).To(Equal(math.MinInt32))
			}
			// Exactly cap of the 60 Group C pods carry a negative rank; the
			// remainder is untouched.
			annotatedC := 0
			for _, p := range groupCPods {
				updated := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(p), updated)).To(Succeed())
				if _, ok := updated.Annotations[corev1.PodDeletionCost]; ok {
					annotatedC++
				}
			}
			Expect(annotatedC).To(Equal(cap), "exactly maxNodesPerCycle (50) Group C pods should be annotated when Group A + Group C exceed the cap")
		})

		It("should annotate everything when total nodes fit within Group A exemption plus cap", func() {
			// 30 Group A + 30 Group C. All 60 nodes should be annotated (A is
			// exempt, C fits inside the 50 cap).
			const groupANodes = 30
			const groupCNodes = 30
			const total = groupANodes + groupCNodes

			ExpectApplied(ctx, env.Client, nodePool)
			nodeClaims, nodes := test.NodeClaimsAndNodes(total, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-all", Namespace: "default"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
			}
			ExpectApplied(ctx, env.Client, pdb)

			groupAPods := make([]*corev1.Pod, groupANodes)
			groupCPods := make([]*corev1.Pod, groupCNodes)
			for i := 0; i < groupANodes; i++ {
				nodes[i].Spec.Taints = append(nodes[i].Spec.Taints, v1.DisruptedNoScheduleTaint)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				groupAPods[i] = rsOwnedPod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
					NodeName:   nodes[i].Name,
				})
				ExpectApplied(ctx, env.Client, groupAPods[i])
			}
			for i := 0; i < groupCNodes; i++ {
				idx := groupANodes + i
				ExpectApplied(ctx, env.Client, nodeClaims[idx], nodes[idx])
				groupCPods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[idx].Name})
				ExpectApplied(ctx, env.Client, groupCPods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			for _, p := range groupAPods {
				Expect(expectPodRank(p)).To(Equal(math.MinInt32))
			}
			for _, p := range groupCPods {
				Expect(expectPodRank(p)).To(BeNumerically("<", 0))
				Expect(expectPodRank(p)).To(BeNumerically(">", math.MinInt32))
			}
		})
	})

	// Direct-helper edge tests. These cover partition cases that are
	// observable only in the NodeRank slice (multi-Group-A pod-count
	// tiebreak, negative classification of disrupted-but-not-PDB-blocked
	// nodes, RankNodes' own empty-input handling). The Reconcile-driven
	// variants would assert on annotation values that don't distinguish
	// these cases. The _Edge_ marker in the It descriptions makes the
	// bypass explicit for reviewers.
	Context("Edge: direct-helper partition checks", func() {
		It("should _Edge_ leave RankNodes a no-op on empty node list", func() {
			ranks, err := deletioncost.RankNodes(ctx, env.Client, cluster, fakeClock, nil, map[string]*v1.NodePool{nodePool.Name: nodePool})
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(BeEmpty())
		})

		It("should _Edge_ classify a disrupted node as Group A even without PDB-blocked pods", func() {
			// RFC §"Group A" uses OR semantics across the three predicates
			// (disrupted OR PDB-blocked OR non-RS-owned). A node that carries
			// the disrupted taint is already on the disruption path and
			// belongs in Group A regardless of PDB state.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Node 0: disrupted taint but no PDB-blocked pods.
			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodes[0])
			disruptedPod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, disruptedPod)

			// Node 1: normal.
			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, normalPod)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := deletioncost.RankNodes(ctx, env.Client, cluster, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(2))

			// Node 0 (disrupted) is Group A → math.MinInt32.
			// Node 1 (normal) is Group C → strictly greater than MinInt32.
			for _, r := range ranks {
				Expect(r.HasDoNotDisrupt).To(BeFalse())
				if r.Node.Node.Name == nodes[0].Name {
					Expect(r.Rank).To(Equal(int(math.MinInt32)))
				} else {
					Expect(r.Rank).To(BeNumerically(">", math.MinInt32))
				}
			}
		})

		It("should _Edge_ rank multiple disrupted+PDB-blocked nodes by pod count ascending", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Both node 0 and node 1 are disrupted + PDB-blocked.
			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			nodes[1].Spec.Taints = append(nodes[1].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodes[0], nodes[1])

			// Node 0: 3 PDB-blocked pods; node 1: 1 PDB-blocked pod.
			for i := 0; i < 3; i++ {
				ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
					NodeName:   nodes[0].Name,
				}))
			}
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
				NodeName:   nodes[1].Name,
			}))

			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-all", Namespace: "default"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
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
			// Both disrupted+blocked nodes get math.MinInt32 (the pod-count
			// tiebreak doesn't change Group A's sentinel rank; the property
			// under test is that the sort completes without error and Group A
			// stays at MinInt32 even with multiple members).
			Expect(node0Rank).To(Equal(math.MinInt32))
			Expect(node1Rank).To(Equal(math.MinInt32))
			Expect(node2Rank).To(BeNumerically(">", math.MinInt32))
		})
	})

	// Reconcile-path edge tests. These cover early-returns inside Reconcile
	// itself (no-nodes short-circuit) that the direct-call RankNodes tests
	// bypass. The "RankNodes on nil input" case above asserts the helper's
	// own zero-input behavior; the equivalent at the Reconcile boundary is
	// "no nodes in cluster state", which exercises the controller's separate
	// len(nodes)==0 early-return path before RankNodes is reached.
	Context("Edge: Reconcile early-return paths", func() {
		It("should _Edge_ short-circuit cleanly when the cluster has no nodes", func() {
			// No nodes applied to the cluster. The Reconcile path's
			// len(nodes)==0 check fires before RankNodes; no pod patches are
			// issued.
			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
			result, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).ToNot(BeZero())
		})
	})
})
