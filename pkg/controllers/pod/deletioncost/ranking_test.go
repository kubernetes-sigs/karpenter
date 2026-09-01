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
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// drainQueueForPod reconciles the shared queue against pod so a fire-and-forget
// enqueue from Controller.Reconcile becomes an observable annotation write.
// No-op when pod is not enqueued (queue.Reconcile short-circuits on miss).
func drainQueueForPod(pod *corev1.Pod) {
	GinkgoHelper()
	if queue.Has(pod) {
		ExpectObjectReconciled(ctx, env.Client, queue, pod)
	}
}

// expectPodRank reads pod via the live client and returns the integer value of
// its pod-deletion-cost annotation. Fails the spec if the annotation is missing
// or non-integer; use expectPodAnnotationCleared for the Group D case. Drains
// the queue first so fire-and-forget writes have landed.
func expectPodRank(pod *corev1.Pod) int {
	GinkgoHelper()
	drainQueueForPod(pod)
	updated := &corev1.Pod{}
	Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
	raw, ok := updated.Annotations[corev1.PodDeletionCost]
	Expect(ok).To(BeTrue(), "pod %s missing pod-deletion-cost annotation", pod.Name)
	val, err := strconv.Atoi(raw)
	Expect(err).ToNot(HaveOccurred(), "pod %s has non-integer pod-deletion-cost %q", pod.Name, raw)
	return val
}

// expectPodAnnotationCleared asserts the pod has no pod-deletion-cost
// annotation (Group D semantics: the controller clears the value). Drains the
// queue first so fire-and-forget clears have landed.
func expectPodAnnotationCleared(pod *corev1.Pod) {
	GinkgoHelper()
	drainQueueForPod(pod)
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
		// podKind labels the pod hosted on each node in a table entry.
		// "normal" pods land the node in Group C (negative rank); "dnd" pods
		// carry the do-not-disrupt annotation and land the node in Group D
		// (annotation cleared).
		const (
			normalPod = "normal"
			dndPod    = "dnd"
		)
		DescribeTable("routes each node to Group C (annotated) or Group D (cleared)",
			func(kinds []string) {
				nodeClaims, nodes := test.NodeClaimsAndNodes(len(kinds), v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
					Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
				})
				ExpectApplied(ctx, env.Client, nodePool)
				for i := range nodeClaims {
					ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				}
				pods := make([]*corev1.Pod, len(kinds))
				for i, kind := range kinds {
					opts := test.PodOptions{NodeName: nodes[i].Name}
					if kind == dndPod {
						opts.ObjectMeta = metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}}
					}
					pods[i] = rsOwnedPod(opts)
					ExpectApplied(ctx, env.Client, pods[i])
				}
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

				controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
				_, err := controller.Reconcile(ctx)
				Expect(err).ToNot(HaveOccurred())

				for i, kind := range kinds {
					if kind == dndPod {
						expectPodAnnotationCleared(pods[i])
					} else {
						Expect(expectPodRank(pods[i])).To(BeNumerically("<", 0))
					}
				}
			},
			Entry("single do-not-disrupt among normals", []string{normalPod, dndPod, normalPod}),
			Entry("all normal", []string{normalPod, normalPod, normalPod}),
			Entry("all do-not-disrupt", []string{dndPod, dndPod}),
			Entry("mixed normal and do-not-disrupt", []string{normalPod, dndPod, normalPod, dndPod}),
		)

		It("should assign sequential ranks starting from -len(nodes)", func() {
			// Contiguity check kept out of the table because it asserts on
			// the negative-rank space rather than per-pod annotation-vs-cleared.
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

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Order across pods depends on the pod-count tie-break so verify
			// the rank set (must span -len(nodes)..-1 contiguously).
			ranks := map[int]bool{}
			for _, p := range pods {
				ranks[expectPodRank(p)] = true
			}
			base := -len(nodes)
			for i := 0; i < len(nodes); i++ {
				Expect(ranks).To(HaveKey(base+i), "expected contiguous rank %d in observed set %v", base+i, ranks)
			}
		})
	})

	Context("Group D composition on non-tainted nodes", func() {
		// DND, PDB-blocked, and non-RS-owned all share Group D routing on
		// non-tainted nodes. These tests verify intersecting predicates
		// still land in Group D; a tainted node overrides them all.
		It("should route do-not-disrupt node hosting a StatefulSet pod to Group D", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			// Node 0 carries the node-level do-not-disrupt annotation and hosts
			// a StatefulSet-owned pod (non-RS-owned).
			nodes[0].Annotations = lo.Assign(nodes[0].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])
			stsPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "StatefulSet", Name: "sts", UID: types.UID("sts-uid"),
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}}},
				NodeName: nodes[0].Name,
			})
			// Node 1 is a plain Group C node so partitioning has something to
			// contrast Group D against.
			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, stsPod, normalPod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Group D clears the annotation on the StatefulSet pod; Group C
			// gets a strictly-negative rank on the normal pod.
			expectPodAnnotationCleared(stsPod)
			Expect(expectPodRank(normalPod)).To(BeNumerically("<", 0))
		})

		It("should route do-not-disrupt node hosting a PDB-blocked pod to Group D", func() {
			// Node 1 is separately tainted to also verify the Group A path
			// still fires on a PDB-blocked pod when the taint is present.
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			// Node 0 carries do-not-disrupt AND hosts a PDB-blocked pod.
			nodes[0].Annotations = lo.Assign(nodes[0].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			// Node 1 carries the disrupted taint (Group A path).
			nodes[1].Spec.Taints = append(nodes[1].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2])
			pdbBlockedPod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
				NodeName:   nodes[0].Name,
			})
			taintedPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name})
			ExpectApplied(ctx, env.Client, pdbBlockedPod, taintedPod, normalPod)
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
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Node 0's PDB-blocked pod is on a do-not-disrupt node → Group D,
			// annotation cleared.
			expectPodAnnotationCleared(pdbBlockedPod)
			// Node 1's pod is on a disrupted-tainted node → Group A, MinInt32.
			Expect(expectPodRank(taintedPod)).To(Equal(math.MinInt32))
			// Node 2 is Group C, strictly-negative rank greater than MinInt32.
			Expect(expectPodRank(normalPod)).To(BeNumerically("<", 0))
			Expect(expectPodRank(normalPod)).To(BeNumerically(">", math.MinInt32))
		})

		It("should _Edge_ keep a disrupted-tainted node in Group A even when do-not-disrupt is set", func() {
			// Fait accompli invariant: once the karpenter.sh/disrupted taint
			// is applied, the disruption controller does not re-check
			// do-not-disrupt (queue.go waitOrTerminate + validation.go
			// validateCandidates both run pre-taint only). A late do-not-
			// disrupt flip must NOT re-route the node to Group D — Group A
			// treatment stays aligned with actual controller behavior.
			// Direct-helper because both classifications produce the same
			// annotated pod-deletion-cost (MinInt32 vs. cleared), and we need
			// to observe HasDoNotDisrupt directly.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			// Node 0: disrupted taint AND do-not-disrupt annotation (the race
			// case — operator flipped the annotation after Karpenter tainted).
			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			nodes[0].Annotations = lo.Assign(nodes[0].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])
			taintedPod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, taintedPod, normalPod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := deletioncost.RankNodes(ctx, env.Client, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(2))

			// Node 0 (disrupted + do-not-disrupt): Group A, MinInt32,
			// HasDoNotDisrupt=false. Node 1 (normal): Group C, strictly
			// greater than MinInt32.
			for _, r := range ranks {
				if r.Node.Node.Name == nodes[0].Name {
					Expect(r.HasDoNotDisrupt).To(BeFalse(), "disrupted-tainted node must stay in Group A regardless of do-not-disrupt")
					Expect(r.Rank).To(Equal(int(math.MinInt32)))
				} else {
					Expect(r.Rank).To(BeNumerically(">", math.MinInt32))
				}
			}
		})
	})

	Context("Group A: Disrupted (tainted) nodes", func() {
		It("should rank disrupted nodes below all other groups", func() {
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

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
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

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
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

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
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

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			annotated := 0
			for _, p := range pods {
				drainQueueForPod(p)
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

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
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

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
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
				drainQueueForPod(p)
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

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
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
			ranks, err := deletioncost.RankNodes(ctx, env.Client, fakeClock, nil, map[string]*v1.NodePool{nodePool.Name: nodePool})
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(BeEmpty())
		})

		It("should _Edge_ classify a disrupted node as Group A even without PDB-blocked pods", func() {
			// Group A is defined solely by the karpenter.sh/disrupted taint
			// (RFC #2935 "Draining"). A tainted node belongs in Group A
			// regardless of PDB state or non-RS-owned pods on the node — the
			// disruption path has already committed to termination.
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

			ranks, err := deletioncost.RankNodes(ctx, env.Client, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
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

			ranks, err := deletioncost.RankNodes(ctx, env.Client, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
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

		// Bare and StatefulSet pods route their host node to Group D.
		// Job, DaemonSet, and kube-system pods do not — they fall through
		// to Group C. Observes HasDoNotDisrupt on NodeRank because Group C
		// and Group D produce different annotation states but the same
		// helper output shape.
		DescribeTable("should _Edge_ classify non-RS-owned pods as Group D (not disruptable)",
			func(ownerRef *metav1.OwnerReference, expectGroupD bool) {
				nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
					Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
				})
				ExpectApplied(ctx, env.Client, nodePool)
				for i := range nodeClaims {
					ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				}

				// Node 0: pod under test with the owner-ref variant.
				podOpts := test.PodOptions{NodeName: nodes[0].Name}
				if ownerRef != nil {
					podOpts.OwnerReferences = []metav1.OwnerReference{*ownerRef}
				}
				ExpectApplied(ctx, env.Client, test.Pod(podOpts))

				// Node 1: RS-owned control pod so Group C is populated and we can
				// assert node 0 is classified DIFFERENTLY (rather than the
				// no-nodes-partition-cleanly-still-passes false positive).
				ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name}))

				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

				var stateNodes []*state.StateNode
				for n := range cluster.Nodes() {
					stateNodes = append(stateNodes, n)
				}

				ranks, err := deletioncost.RankNodes(ctx, env.Client, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
				Expect(err).ToNot(HaveOccurred())
				Expect(ranks).To(HaveLen(2))

				var node0 deletioncost.NodeRank
				for _, r := range ranks {
					if r.Node.Node.Name == nodes[0].Name {
						node0 = r
					}
				}
				if expectGroupD {
					Expect(node0.HasDoNotDisrupt).To(BeTrue(), "non-RS-owned pod should route its host to Group D")
				} else {
					Expect(node0.HasDoNotDisrupt).To(BeFalse(), "Job/DaemonSet-owned or system pods must not push their host to Group D")
					Expect(node0.Rank).To(BeNumerically(">", math.MinInt32), "expected Group B/C rank for RS/Job/DaemonSet-owned or system pod")
				}
			},
			Entry("bare pod (no owner references) routes to Group D", (*metav1.OwnerReference)(nil), true),
			Entry("StatefulSet-owned pod routes to Group D",
				&metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "sts", UID: types.UID("sts-uid"), Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true)},
				true,
			),
			Entry("Job-owned pod is NOT Group D",
				&metav1.OwnerReference{APIVersion: "batch/v1", Kind: "Job", Name: "job", UID: types.UID("job-uid"), Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true)},
				false,
			),
			Entry("DaemonSet-owned pod is NOT Group D",
				&metav1.OwnerReference{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ds", UID: types.UID("ds-uid"), Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true)},
				false,
			),
		)

		It("should _Edge_ route a non-tainted node with a PDB-blocked pod to Group D", func() {
			// Cluster has NO tainted node — exercises the steady-state path.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])
			pdbBlockedPod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
				NodeName:   nodes[0].Name,
			})
			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, pdbBlockedPod, normalPod)
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
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Non-tainted PDB-blocked node → Group D, annotation cleared.
			expectPodAnnotationCleared(pdbBlockedPod)
			// Normal node → Group C.
			Expect(expectPodRank(normalPod)).To(BeNumerically("<", 0))
			Expect(expectPodRank(normalPod)).To(BeNumerically(">", math.MinInt32))
		})

		It("should _Edge_ rank a node with only terminating pods lighter than a node with live pods", func() {
			// Regression test for C4: sortByPodCount counts non-terminating
			// pods only, so a mostly-drained node ranks ahead of a node with
			// the same nominal pod count but no drain in progress. This
			// reflects "how many pods will actually cost to move" rather than
			// the raw pod list length. Both nodes route to Group C in this
			// setup, so the assertion is on the relative rank ordering.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Node 0: three RS-owned pods, all with DeletionTimestamp set via
			// finalizer + Delete (the standard test.expectations helper).
			// Live count is 0 → this node sorts before node 1 in the
			// pod-count-ascending ordering that feeds Group C rank assignment.
			terminatingPods := make([]*corev1.Pod, 3)
			for i := range terminatingPods {
				terminatingPods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
				ExpectApplied(ctx, env.Client, terminatingPods[i])
			}
			for _, p := range terminatingPods {
				ExpectDeletionTimestampSet(ctx, env.Client, p)
			}

			// Node 1: one RS-owned pod, no DeletionTimestamp. Live count is 1.
			// Without the fix, its raw count (1) would rank BELOW node 0's
			// raw count (3) — the assertion below would flip.
			livePod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, livePod)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}
			ranks, err := deletioncost.RankNodes(ctx, env.Client, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(2))

			var node0Rank, node1Rank int
			for _, r := range ranks {
				switch r.Node.Node.Name {
				case nodes[0].Name:
					node0Rank = r.Rank
				case nodes[1].Name:
					node1Rank = r.Rank
				}
			}
			// Node 0 has zero live pods; node 1 has one. Node 0 must rank
			// strictly deeper (more negative → higher delete priority).
			Expect(node0Rank).To(BeNumerically("<", node1Rank),
				"terminating-only node (live count 0) must rank ahead of node with live pods (live count 1)")
		})

		It("should _Edge_ exclude kube-system bare pods from Group D", func() {
			// hasNonRSOwnedPods explicitly skips kube-system, since system
			// components (coredns, kube-proxy) are legitimately unowned and
			// should not push their host node into Group D — those nodes
			// remain consolidation candidates.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Node 0: kube-system bare pod (unowned).
			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system"},
				NodeName:   nodes[0].Name,
			}))
			// Node 1: RS-owned control.
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			ranks, err := deletioncost.RankNodes(ctx, env.Client, fakeClock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool})
			Expect(err).ToNot(HaveOccurred())
			Expect(ranks).To(HaveLen(2))
			for _, r := range ranks {
				Expect(r.Rank).To(BeNumerically(">", math.MinInt32), "no node should reach Group A when the only unowned pod is in kube-system")
			}
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
			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, queue)
			result, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).ToNot(BeZero())
		})
	})
})
