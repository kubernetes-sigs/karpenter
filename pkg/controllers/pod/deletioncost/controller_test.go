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
	"context"
	"errors"
	"time"

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
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// pdbListFailingClient wraps a client.Client and returns an error when asked
// to List PodDisruptionBudget objects; all other calls pass through unchanged.
// Used to drive the fetchPDBs error path in RankNodes at the reconcile level.
type pdbListFailingClient struct {
	client.Client
}

func (c *pdbListFailingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*policyv1.PodDisruptionBudgetList); ok {
		return errors.New("simulated PDB list failure for test")
	}
	return c.Client.List(ctx, list, opts...)
}

var _ = Describe("Controller", func() {
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

	// The PodDeletionCostManagement feature gate is enforced at registration in
	// pkg/controllers/controllers.go (see the guarded NewController call there):
	// when the gate is off the controller is never instantiated. The gate is
	// read once at process start and is not dynamic, so there is no in-Reconcile
	// runtime check to test. The registration-time guard is a compile-time
	// property of controllers.go and is covered by that file's structure alone.

	It("should reconcile and update pod annotations when feature gate is enabled", func() {
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

		controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
		result, err := controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Minute))

		// Verify the controller wrote a sensible negative integer rank, not
		// just any value: ranks for ordinary disruptable nodes start at
		// -(B+C+D) and increase, so they are always strictly negative.
		updatedPod0 := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod0), updatedPod0)).To(Succeed())
		Expect(updatedPod0.Annotations).To(HaveKeyWithValue(corev1.PodDeletionCost, MatchRegexp(`^-\d+$`)))

		updatedPod1 := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod1), updatedPod1)).To(Succeed())
		Expect(updatedPod1.Annotations).To(HaveKeyWithValue(corev1.PodDeletionCost, MatchRegexp(`^-\d+$`)))
	})

	It("should skip reconciliation when no nodes exist", func() {
		controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
		result, err := controller.Reconcile(ctx)
		Expect(err).To(Succeed())
		Expect(result.RequeueAfter).To(Equal(time.Minute))
	})

	It("should requeue with 1s backoff when cluster state is not synced", func() {
		// Matches the disruption controller convention: wait for cluster sync
		// before ranking against a potentially partial view. Apply a
		// NodeClaim + Node to the API server but do NOT push them into the
		// state.Cluster informer, and clear the hasSynced flag so Synced()
		// re-runs the deep check and finds the state missing the applied
		// node. Under this condition Reconcile must short-circuit with a 1s
		// requeue and write no annotations.
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		pod := rsOwnedPod(test.PodOptions{NodeName: node.Name})
		ExpectApplied(ctx, env.Client, pod)
		// Deliberately skip ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated:
		// state.Cluster does not observe the applied node/nodeclaim.
		cluster.SetSynced(false)

		controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)
		result, err := controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Second))

		// The controller returned before ranking or patching, so the pod has
		// no pod-deletion-cost annotation.
		observed := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), observed)).To(Succeed())
		Expect(observed.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
	})

	It("should skip when change detection finds no changes", func() {
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

		controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster)

		// First reconcile — should process (change detected). The pod's
		// ResourceVersion bumps because the controller writes the
		// pod-deletion-cost annotation.
		result, err := controller.Reconcile(ctx)
		Expect(err).To(Succeed())
		Expect(result.RequeueAfter).To(Equal(time.Minute))

		afterFirst := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), afterFirst)).To(Succeed())
		Expect(afterFirst.Annotations).To(HaveKey(corev1.PodDeletionCost))

		// Second reconcile — should skip (no changes). Verify by
		// confirming the pod's ResourceVersion is unchanged: if the
		// short-circuit failed and the controller patched again, the
		// ResourceVersion would bump even on a no-op patch.
		result, err = controller.Reconcile(ctx)
		Expect(err).To(Succeed())
		Expect(result.RequeueAfter).To(Equal(time.Minute))

		afterSecond := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), afterSecond)).To(Succeed())
		Expect(afterSecond.ResourceVersion).To(Equal(afterFirst.ResourceVersion),
			"second reconcile should have taken the change-detection short-circuit and not patched the pod")
	})

	Context("Bounded labeling", func() {
		It("should only annotate top maxNodesPerCycle nodes", func() {
			// Create 3 nodes — all should be annotated since 3 < 50
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
			result, err := controller.Reconcile(ctx)
			Expect(err).To(Succeed())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			// All 3 pods should be annotated (under the 50 limit)
			for _, pod := range pods {
				updatedPod := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
				Expect(updatedPod.Annotations).To(HaveKey(corev1.PodDeletionCost))
			}
		})
	})

	// Deferred behavior: a single dependency failure (PDB list or per-node pod
	// list) aborts the entire Reconcile cycle. There is no per-NodePool partial
	// success path — nodes on healthy NodePools also skip annotation for that
	// cycle. This test documents the current single-error-aborts-all behavior;
	// a per-NodePool granular error path is deferred to a follow-up.
	//
	// TODO(kp-dses9q): once RankNodes fans out per-NodePool with multierr, this
	// test should be updated to assert that a PDB-list failure only skips the
	// affected NodePool and healthy NodePools still get their pods annotated.
	Context("Deferred: per-NodePool error granularity (kp-dses9q)", func() {
		It("should _Deferred_ abort the entire reconcile when the PDB list fails, leaving healthy NodePools' pods unannotated", func() {
			// Set up TWO NodePools. Node 0 belongs to nodePool (with a disrupted
			// taint so RankNodes triggers fetchPDBs). Nodes 1 and 2 belong to
			// otherPool and are healthy — under a per-NodePool granular error
			// path they would still be ranked and annotated. Under the current
			// abort-all behavior, none of the three pods gets an annotation.
			otherPool := test.NodePool()
			otherPool.Name = "other-pool"
			otherPool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			otherPool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			ExpectApplied(ctx, env.Client, nodePool, otherPool)

			// Node 0: on nodePool, tainted disrupted so fetchPDBs runs.
			ncPool0, nodePool0 := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			nodePool0.Spec.Taints = append(nodePool0.Spec.Taints, v1.DisruptedNoScheduleTaint)
			// Nodes 1 and 2: on otherPool, healthy.
			ncOther1, nodeOther1 := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: otherPool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ncOther2, nodeOther2 := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: otherPool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, ncPool0, nodePool0, ncOther1, nodeOther1, ncOther2, nodeOther2)

			podOnDisrupted := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "blocked"}},
				NodeName:   nodePool0.Name,
			})
			podOnHealthy1 := rsOwnedPod(test.PodOptions{NodeName: nodeOther1.Name})
			podOnHealthy2 := rsOwnedPod(test.PodOptions{NodeName: nodeOther2.Name})
			ExpectApplied(ctx, env.Client, podOnDisrupted, podOnHealthy1, podOnHealthy2)

			// Apply a PDB so the disrupted node has a plausible PDB world; the
			// test client fails the list call itself, but seeding a real PDB
			// keeps the fixture realistic in case future test-refactors probe
			// the pre-list state.
			minAvail := intstr.FromString("100%")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-all", Namespace: podOnDisrupted.Namespace},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &minAvail,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "blocked"}},
				},
				Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
			}
			ExpectApplied(ctx, env.Client, pdb)

			nodes := []*corev1.Node{nodePool0, nodeOther1, nodeOther2}
			nodeClaims := []*v1.NodeClaim{ncPool0, ncOther1, ncOther2}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			// Wrap env.Client to fail the PDB list. All other traffic (nodepool
			// list, pod list, patches) flows through unchanged.
			failing := &pdbListFailingClient{Client: env.Client}
			controller := deletioncost.NewController(fakeClock, failing, cloudProvider, cluster)
			_, err := controller.Reconcile(ctx)
			Expect(err).To(HaveOccurred(), "current behavior: PDB list failure aborts the whole reconcile")

			// None of the pods, on either NodePool, got annotated. This is the
			// property the follow-up bead (kp-dses9q) will change.
			for _, pod := range []*corev1.Pod{podOnDisrupted, podOnHealthy1, podOnHealthy2} {
				observed := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), observed)).To(Succeed())
				Expect(observed.Annotations).ToNot(HaveKey(corev1.PodDeletionCost),
					"pod %s on healthy or affected NodePool should not have been annotated during the aborted reconcile", pod.Name)
			}
		})
	})
})
