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
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

func drainQueueForPod(pod *corev1.Pod) {
	GinkgoHelper()
	if queue.Has(pod) {
		ExpectObjectReconciled(ctx, env.Client, queue, pod)
	}
}

// Fails if the annotation is absent; use expectPodAnnotationCleared for that.
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

func expectPodAnnotationCleared(pod *corev1.Pod) {
	GinkgoHelper()
	drainQueueForPod(pod)
	updated := &corev1.Pod{}
	Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
	Expect(updated.Annotations).ToNot(HaveKey(corev1.PodDeletionCost),
		"pod %s should not carry pod-deletion-cost", pod.Name)
}

var _ = Describe("Ranking", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
		// test.NodePool() leaves ConsolidateAfter nil and Budgets unset, which routes
		// every node to Group D. Set permissive defaults so the specs below exercise
		// partitioning rather than the disabled and budget-overflow paths.
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
	})

	Context("Two-tier partitioning", func() {
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

				controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
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

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
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
		It("should route do-not-disrupt node hosting a StatefulSet pod to Group D", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			nodes[0].Annotations = lo.Assign(nodes[0].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])
			stsPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "StatefulSet", Name: "sts", UID: types.UID("sts-uid"),
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}}},
				NodeName: nodes[0].Name,
			})
			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, stsPod, normalPod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			expectPodAnnotationCleared(stsPod)
			Expect(expectPodRank(normalPod)).To(BeNumerically("<", 0))
		})

		It("should route do-not-disrupt node hosting a PDB-blocked pod to Group D", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			nodes[0].Annotations = lo.Assign(nodes[0].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
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

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			expectPodAnnotationCleared(pdbBlockedPod)
			Expect(expectPodRank(taintedPod)).To(Equal(math.MinInt32))
			Expect(expectPodRank(normalPod)).To(BeNumerically("<", 0))
			Expect(expectPodRank(normalPod)).To(BeNumerically(">", math.MinInt32))
		})

		It("should _Edge_ keep a disrupted-tainted node in Group A even when do-not-disrupt is set", func() {
			// Once the disrupted taint is applied the disruption controller stops
			// re-checking do-not-disrupt, so a late flip must not re-route to Group D.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
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

			groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool}, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(totalRanked(groupA, groupBC, groupD)).To(Equal(2))

			info0 := rankInfoFor(nodes[0].Name, groupA, groupBC, groupD)
			Expect(info0.found).To(BeTrue())
			Expect(info0.cleanup).To(BeFalse(), "disrupted-tainted node must stay in Group A regardless of do-not-disrupt")
			Expect(info0.rank).To(Equal(int(math.MinInt32)))

			info1 := rankInfoFor(nodes[1].Name, groupA, groupBC, groupD)
			Expect(info1.found).To(BeTrue())
			Expect(info1.rank).To(BeNumerically(">", math.MinInt32))
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

			pod1 := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			pod2 := rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name})
			dndPod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
				NodeName:   nodes[3].Name,
			})
			ExpectApplied(ctx, env.Client, pod1, pod2, dndPod)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

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

			nodeClaims[1].StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			ExpectApplied(ctx, env.Client, nodeClaims[1])
			driftedPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, driftedPod)

			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name})
			ExpectApplied(ctx, env.Client, normalPod)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			Expect(expectPodRank(pdbBlockedPod)).To(Equal(math.MinInt32))
			Expect(expectPodRank(driftedPod)).To(BeNumerically("<", expectPodRank(normalPod)))
		})

		It("should not annotate pods on an unmanaged node that is being deleted", func() {
			// StateNode.Deleted() is true for a NodeClaim-less node with a deletion
			// timestamp, and classifyNode reaches isGoingAway before
			// ValidateNodeDisruptable can reject it, so without the Managed()
			// filter this node lands in Group A.
			node := test.Node(test.NodeOptions{
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
			})
			ExpectApplied(ctx, env.Client, nodePool, node)
			pod := rsOwnedPod(test.PodOptions{NodeName: node.Name})
			ExpectApplied(ctx, env.Client, pod)

			// No NodeClaim is ever created for this node, so Managed() is false.
			ExpectDeletionTimestampSet(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			stateNode := ExpectStateNodeExists(cluster, node)
			Expect(stateNode.Managed()).To(BeFalse())
			Expect(stateNode.Deleted()).To(BeTrue())

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			expectPodAnnotationCleared(pod)
		})
	})

	Context("Per-NodePool budgets", func() {
		It("should respect per-NodePool consolidation budgets across multiple pools", func() {
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

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			Expect(expectPodRank(podA)).To(BeNumerically("<", 0))
			expectPodAnnotationCleared(podB)
		})

		It("should send drifted-node overflow past the drift budget to Group D", func() {
			// Drift twin of the consolidation-budget spec; covers the driftOverflow branch.
			pool := test.NodePool()
			pool.Name = "drift-pool"
			pool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			pool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "1"}}
			ExpectApplied(ctx, env.Client, pool)

			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: pool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			for i := range nodeClaims {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			}
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			podFirst := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			podSecond := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, podFirst, podSecond)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			// Which node lands in B vs D depends on the sort tie-break, so assert on the set.
			drainQueueForPod(podFirst)
			drainQueueForPod(podSecond)
			updated := make([]*corev1.Pod, 2)
			for i, p := range []*corev1.Pod{podFirst, podSecond} {
				updated[i] = &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(p), updated[i])).To(Succeed())
			}
			var ranked, cleared int
			for _, u := range updated {
				if _, ok := u.Annotations[corev1.PodDeletionCost]; ok {
					ranked++
				} else {
					cleared++
				}
			}
			Expect(ranked).To(Equal(1), "exactly one drifted node should fit inside the Nodes=\"1\" drift budget (Group B)")
			Expect(cleared).To(Equal(1), "the second drifted node should overflow to Group D and clear its pod's annotation")
		})
	})

	Context("ConsolidateAfter=nil (consolidation disabled)", func() {
		// Unlike the outer BeforeEach, this Context leaves ConsolidateAfter unset so
		// isConsolidationDisabled fires.
		It("should route a nil-ConsolidateAfter pool to Group D while a 0s pool stays in Group C", func() {
			nilPool := test.NodePool()
			nilPool.Name = "nil-consolidate-pool"
			nilPool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}

			activePool := test.NodePool()
			activePool.Name = "active-consolidate-pool"
			activePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			activePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			ExpectApplied(ctx, env.Client, nilPool, activePool)

			ncNil, nNil := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nilPool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ncActive, nActive := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: activePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, ncNil, nNil, ncActive, nActive)
			podNil := rsOwnedPod(test.PodOptions{NodeName: nNil.Name})
			podActive := rsOwnedPod(test.PodOptions{NodeName: nActive.Name})
			ExpectApplied(ctx, env.Client, podNil, podActive)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nNil, nActive}, []*v1.NodeClaim{ncNil, ncActive})

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			expectPodAnnotationCleared(podNil)
			Expect(expectPodRank(podActive)).To(BeNumerically("<", 0))
		})

		It("should route a drifted node in a nil-ConsolidateAfter pool to Group B (drift beats consolidation-disabled)", func() {
			// Regression: isConsolidationDisabled used to run before isDrifted, which sent
			// drifted nodes in ConsolidateAfter=nil pools to Group D.
			driftedPool := test.NodePool()
			driftedPool.Name = "drifted-nil-consolidate-pool"
			driftedPool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			ExpectApplied(ctx, env.Client, driftedPool)

			nc, node := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: driftedPool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			nc.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			ExpectApplied(ctx, env.Client, nc, node)
			pod := rsOwnedPod(test.PodOptions{NodeName: node.Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nc})

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())
			ExpectObjectReconciled(ctx, env.Client, queue, pod)

			Expect(expectPodRank(pod)).To(BeNumerically("<", 0))
		})

		It("should route a drifted node in a static NodePool to Group C (not Group B)", func() {
			// nodePoolMap is hand-built because CRD validation forbids most disruption
			// fields on static NodePools.
			activePool := test.NodePool()
			activePool.Name = "active-pool-drift-static"
			activePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			activePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			ExpectApplied(ctx, env.Client, activePool)

			ncActive, nodeActive := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: activePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ncActive.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			const staticName = "static-pool-drift-static"
			ncStatic, nodeStatic := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: staticName}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ncStatic.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			ExpectApplied(ctx, env.Client, ncActive, nodeActive, ncStatic, nodeStatic)
			podActive := rsOwnedPod(test.PodOptions{NodeName: nodeActive.Name})
			podStatic := rsOwnedPod(test.PodOptions{NodeName: nodeStatic.Name})
			ExpectApplied(ctx, env.Client, podActive, podStatic)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodeActive, nodeStatic}, []*v1.NodeClaim{ncActive, ncStatic})

			nodePoolMap, nodePoolToInstanceTypesMap, err := disruption.BuildNodePoolMap(ctx, env.Client, cloudProvider)
			Expect(err).ToNot(HaveOccurred())
			staticPool := &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{Name: staticName},
				Spec: v1.NodePoolSpec{
					Replicas: lo.ToPtr(int64(1)),
					Template: activePool.Spec.Template,
				},
			}
			nodePoolMap[staticName] = staticPool
			nodePoolToInstanceTypesMap[staticName] = nodePoolToInstanceTypesMap[activePool.Name]

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}
			groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, stateNodes, nodePoolMap, nodePoolToInstanceTypesMap)
			Expect(err).ToNot(HaveOccurred())

			infoActive := rankInfoFor(nodeActive.Name, groupA, groupBC, groupD)
			infoStatic := rankInfoFor(nodeStatic.Name, groupA, groupBC, groupD)

			Expect(infoActive.found).To(BeTrue())
			Expect(infoActive.cleanup).To(BeFalse())
			Expect(len(groupBC)).ToNot(Equal(0))
			Expect(groupBC[0].Node.Name).To(Equal(nodeActive.Name),
				"non-static drifted node must land at the front of Group B")

			Expect(infoStatic.found).To(BeTrue())
			if !infoStatic.cleanup {
				Expect(infoStatic.rank).To(BeNumerically(">", infoActive.rank),
					"static-owned drifted node must never rank ahead of a real Group B candidate")
			}
		})
	})

	Context("Bounded labeling: cap applies to Groups B/C/D only", func() {
		// Group A is exempt from maxNodesPerCycle.
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

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
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
			const groupANodes = 60
			const groupCNodes = 3
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

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
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

		It("should exempt Group A from the cap and truncate only Group C overflow", func() {
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

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			for _, p := range groupAPods {
				Expect(expectPodRank(p)).To(Equal(math.MinInt32))
			}
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

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
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

	Context("Edge: direct-helper partition checks", func() {
		It("should _Edge_ leave RankNodes a no-op on empty node list", func() {
			groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, nil, map[string]*v1.NodePool{nodePool.Name: nodePool}, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(totalRanked(groupA, groupBC, groupD)).To(Equal(0))
		})

		It("should _Edge_ classify a disrupted node as Group A even without PDB-blocked pods", func() {
			// The disrupted taint alone defines Group A, whatever else is on the node.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodes[0])
			disruptedPod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, disruptedPod)

			normalPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, normalPod)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool}, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(totalRanked(groupA, groupBC, groupD)).To(Equal(2))

			info0 := rankInfoFor(nodes[0].Name, groupA, groupBC, groupD)
			Expect(info0.found).To(BeTrue())
			Expect(info0.cleanup).To(BeFalse())
			Expect(info0.rank).To(Equal(int(math.MinInt32)))

			info1 := rankInfoFor(nodes[1].Name, groupA, groupBC, groupD)
			Expect(info1.found).To(BeTrue())
			Expect(info1.cleanup).To(BeFalse())
			Expect(info1.rank).To(BeNumerically(">", math.MinInt32))
		})

		It("should _Edge_ keep every disrupted+PDB-blocked node at MinInt32 regardless of relative sort order", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
			nodes[1].Spec.Taints = append(nodes[1].Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodes[0], nodes[1])

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

			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[2].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool}, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(totalRanked(groupA, groupBC, groupD)).To(Equal(3))

			info0 := rankInfoFor(nodes[0].Name, groupA, groupBC, groupD)
			info1 := rankInfoFor(nodes[1].Name, groupA, groupBC, groupD)
			info2 := rankInfoFor(nodes[2].Name, groupA, groupBC, groupD)
			Expect(info0.found).To(BeTrue())
			Expect(info1.found).To(BeTrue())
			Expect(info2.found).To(BeTrue())
			Expect(info0.rank).To(Equal(math.MinInt32))
			Expect(info1.rank).To(Equal(math.MinInt32))
			Expect(info2.rank).To(BeNumerically(">", math.MinInt32))
		})

		// Bare and StatefulSet pods route the node to Group D; Job, DaemonSet and
		// kube-system pods fall through to Group C.
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

				podOpts := test.PodOptions{NodeName: nodes[0].Name}
				if ownerRef != nil {
					podOpts.OwnerReferences = []metav1.OwnerReference{*ownerRef}
				}
				ExpectApplied(ctx, env.Client, test.Pod(podOpts))

				ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name}))

				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

				var stateNodes []*state.StateNode
				for n := range cluster.Nodes() {
					stateNodes = append(stateNodes, n)
				}

				groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool}, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(totalRanked(groupA, groupBC, groupD)).To(Equal(2))

				info0 := rankInfoFor(nodes[0].Name, groupA, groupBC, groupD)
				Expect(info0.found).To(BeTrue())
				if expectGroupD {
					Expect(info0.cleanup).To(BeTrue(), "non-RS-owned pod should route its host to Group D")
				} else {
					Expect(info0.cleanup).To(BeFalse(), "Job/DaemonSet-owned or system pods must not push their host to Group D")
					Expect(info0.rank).To(BeNumerically(">", math.MinInt32), "expected Group B/C rank for RS/Job/DaemonSet-owned or system pod")
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

		// isUnpriceable has two branches that return true and the suite covered
		// neither: a NodePool absent from the instance-type map, and a node whose
		// instance-type label is absent from its NodePool's map. The priced node
		// is the control, so this also shows the filter is selective rather than
		// routing every node to cleanup.
		It("should _Edge_ route an unpriceable node to Group D and leave a priced node ranked", func() {
			const pricedIT, absentIT, zone, ct = "priced-it", "absent-it", "test-zone-1", v1.CapacityTypeOnDemand
			unmappedPool := test.NodePool()
			unmappedPool.Name = "pool-absent-from-instance-type-map"
			unmappedPool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			unmappedPool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			ExpectApplied(ctx, env.Client, nodePool, unmappedPool)

			newNode := func(poolName, itName string) (*v1.NodeClaim, *corev1.Node) {
				return test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
						v1.NodePoolLabelKey:            poolName,
						corev1.LabelInstanceTypeStable: itName,
						corev1.LabelTopologyZone:       zone,
						v1.CapacityTypeLabelKey:        ct,
					}},
					Status: v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
				})
			}
			ncPriced, nodePriced := newNode(nodePool.Name, pricedIT)
			ncAbsentIT, nodeAbsentIT := newNode(nodePool.Name, absentIT)
			ncAbsentPool, nodeAbsentPool := newNode(unmappedPool.Name, pricedIT)

			allNodeClaims := []*v1.NodeClaim{ncPriced, ncAbsentIT, ncAbsentPool}
			allNodes := []*corev1.Node{nodePriced, nodeAbsentIT, nodeAbsentPool}
			for i := range allNodes {
				ExpectApplied(ctx, env.Client, allNodeClaims[i], allNodes[i])
				ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: allNodes[i].Name}))
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, allNodes, allNodeClaims)

			// unmappedPool is deliberately absent from itMap, and nodePool's entry
			// deliberately omits absentIT.
			itMap := map[string]map[string]*cloudprovider.InstanceType{
				nodePool.Name: {pricedIT: &cloudprovider.InstanceType{
					Name: pricedIT,
					Offerings: cloudprovider.Offerings{{
						Available: true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{
							v1.CapacityTypeLabelKey:  ct,
							corev1.LabelTopologyZone: zone,
						}),
						Price: 1.0,
					}},
				}},
			}
			nodePoolMap := map[string]*v1.NodePool{nodePool.Name: nodePool, unmappedPool.Name: unmappedPool}

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}
			groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, stateNodes, nodePoolMap, itMap)
			Expect(err).ToNot(HaveOccurred())
			Expect(totalRanked(groupA, groupBC, groupD)).To(Equal(3))

			infoPriced := rankInfoFor(nodePriced.Name, groupA, groupBC, groupD)
			Expect(infoPriced.found).To(BeTrue())
			Expect(infoPriced.cleanup).To(BeFalse(), "a node with a resolvable offering price must stay in Group B/C")

			for _, tc := range []struct {
				name   string
				reason string
			}{
				{nodeAbsentIT.Name, "a node whose instance-type label is absent from its NodePool's map is unpriceable and must land in Group D"},
				{nodeAbsentPool.Name, "a node whose NodePool is absent from the instance-type map is unpriceable and must land in Group D"},
			} {
				info := rankInfoFor(tc.name, groupA, groupBC, groupD)
				Expect(info.found).To(BeTrue())
				Expect(info.cleanup).To(BeTrue(), tc.reason)
			}
		})

		It("should _Edge_ route a non-tainted node with a PDB-blocked pod to Group D", func() {
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

			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())

			expectPodAnnotationCleared(pdbBlockedPod)
			Expect(expectPodRank(normalPod)).To(BeNumerically("<", 0))
			Expect(expectPodRank(normalPod)).To(BeNumerically(">", math.MinInt32))
		})

		It("should _Edge_ rank a node with only terminating pods ahead of a node with live pods (higher SavingsRatio)", func() {
			// RescheduleDisruptionCost counts only pod.IsReschedulable pods, so at equal
			// price the drained node's base-1.0 floor gives the higher ratio and the
			// deeper rank. Terminating RS pods, DaemonSet pods and node-owned pods drop out.
			const it, zone, ct = "test-it", "test-zone-1", v1.CapacityTypeOnDemand
			nodeLabels := map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: it,
				corev1.LabelTopologyZone:       zone,
				v1.CapacityTypeLabelKey:        ct,
			}
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: nodeLabels},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			terminatingPods := make([]*corev1.Pod, 3)
			for i := range terminatingPods {
				terminatingPods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
				ExpectApplied(ctx, env.Client, terminatingPods[i])
			}
			for _, p := range terminatingPods {
				ExpectDeletionTimestampSet(ctx, env.Client, p)
			}

			livePod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, livePod)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			// Fake instance-type map so ResolveOfferingPrice returns a positive
			// price for both nodes; otherwise Price=0 collapses both ratios to 0
			// and the tie-break drops to node name, which is non-deterministic.
			itMap := map[string]map[string]*cloudprovider.InstanceType{
				nodePool.Name: {it: &cloudprovider.InstanceType{
					Name: it,
					Offerings: cloudprovider.Offerings{{
						Available: true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{
							v1.CapacityTypeLabelKey:  ct,
							corev1.LabelTopologyZone: zone,
						}),
						Price: 1.0,
					}},
				}},
			}

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}
			groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool}, itMap)
			Expect(err).ToNot(HaveOccurred())
			Expect(totalRanked(groupA, groupBC, groupD)).To(Equal(2))

			info0 := rankInfoFor(nodes[0].Name, groupA, groupBC, groupD)
			info1 := rankInfoFor(nodes[1].Name, groupA, groupBC, groupD)
			Expect(info0.found).To(BeTrue())
			Expect(info1.found).To(BeTrue())
			Expect(info0.rank).To(BeNumerically("<", info1.rank),
				"terminating-only node (higher SavingsRatio) must rank ahead of node with live reschedulable pod (lower ratio)")
		})

		It("should _Edge_ exclude kube-system bare pods from Group D", func() {
			// kube-system pods are legitimately unowned, so they must not push the node to D.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system"},
				NodeName:   nodes[0].Name,
			}))
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name}))

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			groupA, groupBC, groupD, err := deletioncost.RankNodes(ctx, env.Client, env.Clock, stateNodes, map[string]*v1.NodePool{nodePool.Name: nodePool}, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(totalRanked(groupA, groupBC, groupD)).To(Equal(2))
			Expect(groupA).To(BeEmpty(), "kube-system bare pods must not push a node to Group A")
		})
	})

	Context("Edge: Reconcile early-return paths", func() {
		It("should _Edge_ short-circuit cleanly when the cluster has no nodes", func() {
			controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
			result, err := controller.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).ToNot(BeZero())
		})
	})
})
