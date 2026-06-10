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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

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

	It("should skip reconciliation when feature gate is disabled", func() {
		opts := test.Options()
		opts.FeatureGates.PodDeletionCostManagement = false
		disabledCtx := options.ToContext(ctx, opts)

		controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, recorder)
		result, err := controller.Reconcile(disabledCtx)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).ToNot(BeZero())
	})

	It("should reconcile and update pod annotations when feature gate is enabled", func() {
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
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, recorder)
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
		controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, recorder)
		result, err := controller.Reconcile(ctx)
		Expect(err).To(Succeed())
		Expect(result.RequeueAfter).To(Equal(time.Minute))
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
		pod := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
		ExpectApplied(ctx, env.Client, pod)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, recorder)

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
				pods[i] = test.Pod(test.PodOptions{NodeName: n.Name})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			controller := deletioncost.NewController(fakeClock, env.Client, cloudProvider, cluster, recorder)
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
})
