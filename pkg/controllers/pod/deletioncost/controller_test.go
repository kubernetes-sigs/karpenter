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
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

type pdbListFailingClient struct {
	client.Client
}

func (c *pdbListFailingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*policyv1.PodDisruptionBudgetList); ok {
		return errors.New("simulated PDB list failure for test")
	}
	return c.Client.List(ctx, list, opts...)
}

type toggleablePDBListFailingClient struct {
	client.Client
	fail bool
}

func (c *toggleablePDBListFailingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.fail {
		if _, ok := list.(*policyv1.PodDisruptionBudgetList); ok {
			return errors.New("simulated PDB list failure for test")
		}
	}
	return c.Client.List(ctx, list, opts...)
}

var _ = Describe("Controller", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
		// Without these, test.NodePool()'s unset Disruption fields route every
		// node to Group D: nil ConsolidateAfter reads as consolidation-disabled,
		// and the Budgets CRD default of 10% caps Groups B and C at 1 slot.
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
	})

	// No gate test here on purpose: PodDeletionCostManagement is enforced at
	// registration in pkg/controllers/controllers.go, read once at process start,
	// so when it is off the controller is never instantiated and there is no
	// in-Reconcile check to exercise.

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

		controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
		result, err := controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		ExpectObjectReconciled(ctx, env.Client, queue, pod0)
		ExpectObjectReconciled(ctx, env.Client, queue, pod1)

		// Pin the exact rank set so a swap or a dropped node surfaces here.
		updatedPod0 := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod0), updatedPod0)).To(Succeed())
		updatedPod1 := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod1), updatedPod1)).To(Succeed())
		ranks := []string{
			updatedPod0.Annotations[corev1.PodDeletionCost],
			updatedPod1.Annotations[corev1.PodDeletionCost],
		}
		Expect(ranks).To(ConsistOf("-1", "-2"))

		Expect(queue.Has(pod0)).To(BeFalse())
		Expect(queue.Has(pod1)).To(BeFalse())
	})

	It("should only annotate pods whose controller owner reference is a ReplicaSet", func() {
		// The Job pod is the load-bearing fixture: hasPinningPods tolerates Job,
		// so the node stays in Groups B/C and the enqueue gate is what excludes
		// the pod, not the partition step.
		nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0])

		rsPod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
		jobPod := test.Pod(test.PodOptions{
			NodeName: nodes[0].Name,
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "test-job", UID: types.UID("test-job-uid"),
				Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
			}}},
		})
		ExpectApplied(ctx, env.Client, rsPod, jobPod)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
		_, err := controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())

		Expect(queue.Has(rsPod)).To(BeTrue())
		Expect(queue.Has(jobPod)).To(BeFalse())

		ExpectObjectReconciled(ctx, env.Client, queue, rsPod)
		for _, expectation := range []struct {
			pod       *corev1.Pod
			annotated bool
		}{
			{pod: rsPod, annotated: true},
			{pod: jobPod, annotated: false},
		} {
			observed := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(expectation.pod), observed)).To(Succeed())
			if expectation.annotated {
				Expect(observed.Annotations).To(HaveKeyWithValue(corev1.PodDeletionCost, "-1"))
			} else {
				Expect(observed.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
			}
		}
	})

	// A pod whose only ReplicaSet reference is not the controller reference is in
	// the same position as a bare pod: no controller claims it, so nothing would
	// recreate it elsewhere and it pins its node. hasPinningPods reads ownership
	// through the controller reference, so the node routes to Group D and its
	// ReplicaSet-controlled pods have their annotations cleared instead of ranked.
	It("should treat a pod with a non-controller ReplicaSet reference as pinning its node", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])

		// Pre-annotated so a Group D route is observable as a clear, not as an
		// absence that a skipped node would also produce.
		pinnedNodeRSPod := rsOwnedPod(test.PodOptions{
			NodeName:   nodes[0].Name,
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{corev1.PodDeletionCost: "-5"}},
		})
		nonControllerRSPod := test.Pod(test.PodOptions{
			NodeName: nodes[0].Name,
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "test-rs", UID: types.UID("test-rs-uid"),
			}}},
		})
		controlPod := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
		ExpectApplied(ctx, env.Client, pinnedNodeRSPod, nonControllerRSPod, controlPod)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
		_, err := controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())

		expectPodAnnotationCleared(pinnedNodeRSPod)
		// The gate never annotates the non-controller pod either way.
		expectPodAnnotationCleared(nonControllerRSPod)
		Expect(expectPodRank(controlPod)).To(BeNumerically("<", 0),
			"the node with no pinning pod must still rank, so the Group D route is caused by the non-controller reference")
	})

	It("should not consume a per-cycle slot for a node hosting no ReplicaSet-controlled pods", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0])

		jobPod := test.Pod(test.PodOptions{
			NodeName: nodes[0].Name,
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "test-job", UID: types.UID("test-job-uid"),
				Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
			}}},
		})
		ExpectApplied(ctx, env.Client, jobPod)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
		_, err := controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())

		Expect(queue.Has(jobPod)).To(BeFalse())
		// The metric is Reset then Set only for non-zero pools, so "not counted"
		// is an absent series rather than a zero sample.
		_, found := FindMetricWithLabelValues(
			"karpenter_pod_deletion_cost_nodes_with_pending_annotation_writes",
			map[string]string{metrics.NodePoolLabel: nodePool.Name},
		)
		Expect(found).To(BeFalse(), "a node with no ReplicaSet-controlled pods must not be counted as enqueued work")
	})

	It("should not advance the consolidation cursor when the cluster is empty", func() {
		// Regression: the len(nodes)==0 short-circuit must not advance
		// lastConsolidationState, or the first cycle after nodes appear takes the
		// "unchanged" short-circuit and never ranks.
		controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
		result, err := controller.Reconcile(ctx)
		Expect(err).To(Succeed())
		Expect(result.RequeueAfter).To(Equal(time.Minute))

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

		_, err = controller.Reconcile(ctx)
		Expect(err).To(Succeed())
		ExpectObjectReconciled(ctx, env.Client, queue, pod)

		observed := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), observed)).To(Succeed())
		Expect(observed.Annotations).To(HaveKey(corev1.PodDeletionCost),
			"second reconcile must rank the newly-added node; if the cursor were advanced by the empty first reconcile, the second would short-circuit")
	})

	It("should requeue with 1s backoff when cluster state is not synced", func() {
		// Applies a NodeClaim + Node to the API server without pushing them into
		// the state.Cluster informer, then clears hasSynced so Synced() re-runs
		// its deep check and finds state missing the applied node.
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		pod := rsOwnedPod(test.PodOptions{NodeName: node.Name})
		ExpectApplied(ctx, env.Client, pod)
		// Skipping ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated is the
		// point: state.Cluster must not observe the applied node/nodeclaim.
		cluster.SetSynced(false)

		controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
		result, err := controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Second))

		observed := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), observed)).To(Succeed())
		Expect(observed.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
	})

	It("should retry on the same state after a failed reconcile (skip cursor is not advanced on error)", func() {
		// Regression: the cursor used to be advanced at the unchanged-state check,
		// ahead of the rest of Reconcile, so a mid-reconcile error left it
		// advanced and the retry was silently dropped. The disrupted taint is
		// what makes RankNodes reach the failing PDB list.
		nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		nodes[0].Spec.Taints = append(nodes[0].Spec.Taints, v1.DisruptedNoScheduleTaint)
		ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0])
		pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
		ExpectApplied(ctx, env.Client, pod)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		failing := &toggleablePDBListFailingClient{Client: env.Client, fail: true}
		controller := deletioncost.NewController(env.Clock, failing, cloudProvider, cluster, queue)

		_, err := controller.Reconcile(ctx)
		Expect(err).To(HaveOccurred())

		// Reuses the SAME controller instance so lastConsolidationState survives.
		failing.fail = false
		_, err = controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())
		ExpectObjectReconciled(ctx, env.Client, queue, pod)

		observed := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), observed)).To(Succeed())
		Expect(observed.Annotations).To(HaveKey(corev1.PodDeletionCost),
			"second reconcile must succeed after the first failed one; if it took the unchanged short-circuit, the cursor was advanced on error")
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

		controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)

		result, err := controller.Reconcile(ctx)
		Expect(err).To(Succeed())
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		ExpectObjectReconciled(ctx, env.Client, queue, pod)

		afterFirst := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), afterFirst)).To(Succeed())
		Expect(afterFirst.Annotations).To(HaveKey(corev1.PodDeletionCost))

		// An unchanged ResourceVersion is the evidence that no second patch fired.
		result, err = controller.Reconcile(ctx)
		Expect(err).To(Succeed())
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(queue.Has(pod)).To(BeFalse())

		afterSecond := &corev1.Pod{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), afterSecond)).To(Succeed())
		Expect(afterSecond.ResourceVersion).To(Equal(afterFirst.ResourceVersion),
			"second reconcile should have taken the change-detection short-circuit and not enqueued the pod")
	})

	// Cap-boundary cases live in ranking_test.go's "Bounded labeling" Context;
	// repeating them here would only re-test that 3 < 50.

	// Documents current behavior, not desired behavior: one dependency failure
	// aborts the whole cycle, so healthy NodePools skip annotation too. If the
	// deferred per-NodePool granular error path lands, update this to assert
	// healthy pools still get annotated.
	Context("Deferred: per-NodePool error granularity", func() {
		It("should _Deferred_ abort the entire reconcile when the PDB list fails, leaving healthy NodePools' pods unannotated", func() {
			// Two pools: node 0 carries the disrupted taint so RankNodes reaches
			// the PDB list, and otherPool's two nodes are the healthy ones a
			// granular error path would still annotate.
			otherPool := test.NodePool()
			otherPool.Name = "other-pool"
			otherPool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			otherPool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			ExpectApplied(ctx, env.Client, nodePool, otherPool)

			ncPool0, nodePool0 := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			nodePool0.Spec.Taints = append(nodePool0.Spec.Taints, v1.DisruptedNoScheduleTaint)
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

			failing := &pdbListFailingClient{Client: env.Client}
			controller := deletioncost.NewController(env.Clock, failing, cloudProvider, cluster, queue)
			_, err := controller.Reconcile(ctx)
			Expect(err).To(HaveOccurred(), "current behavior: PDB list failure aborts the whole reconcile")

			// Stays silent on podOnHealthy1/2 on purpose: asserting the abort-all
			// shape for healthy pools would fire as a false regression the day the
			// granular path lands.
			observedDisrupted := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(podOnDisrupted), observedDisrupted)).To(Succeed())
			Expect(observedDisrupted.Annotations).ToNot(HaveKey(corev1.PodDeletionCost),
				"pod on the affected NodePool must not be annotated when its NodePool aborts")
			_ = podOnHealthy1
			_ = podOnHealthy2
		})
	})
})
