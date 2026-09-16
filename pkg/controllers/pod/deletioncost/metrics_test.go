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
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// podLabelsUpdatedDelta reads the current value of pod_labels_updated_total for the given
// label combination, returning 0 when the collector has not yet emitted a
// sample. crmetrics.Registry is process-global so counter values accumulate
// across specs; capture pre, run the scenario, and assert on the delta.
func podLabelsUpdatedDelta(labels map[string]string) float64 {
	GinkgoHelper()
	metric, ok := FindMetricWithLabelValues("karpenter_pod_deletion_cost_pod_labels_updated_total", labels)
	if !ok || metric == nil {
		return 0
	}
	return lo.FromPtr(metric.Counter.Value)
}

var _ = Describe("Metrics", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
	})

	It("should set nodes_ranked to the number of nodes ranked in the last reconcile", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		for i := range nodeClaims {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		for i := range nodes {
			ExpectApplied(ctx, env.Client, rsOwnedPod(test.PodOptions{NodeName: nodes[i].Name}))
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		controller := deletioncost.NewController(env.Clock, env.Client, cloudProvider, cluster, queue)
		_, err := controller.Reconcile(ctx)
		Expect(err).ToNot(HaveOccurred())

		// nodesRanked is a Set() gauge, reset each cycle. All three nodes
		// share the same nodepool.
		ExpectMetricGaugeValue(deletioncost.NodesRankedMetric, 3, map[string]string{metrics.NodePoolLabel: nodePool.Name})
	})

	It("should increment pod_labels_updated_total{result=updated} on a successful annotation write", func() {
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

		before := podLabelsUpdatedDelta(map[string]string{deletioncost.ResultLabel: deletioncost.ResultUpdated})
		queue.Add(pod, -13, false)
		ExpectObjectReconciled(ctx, env.Client, queue, pod)
		after := podLabelsUpdatedDelta(map[string]string{deletioncost.ResultLabel: deletioncost.ResultUpdated})
		Expect(after-before).To(Equal(1.0),
			"pod_labels_updated_total{result=updated} should increment on a successful patch")
	})

	It("should increment pod_labels_updated_total{result=error} when the patch surfaces a retryable error", func() {
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

		throttler := newThrottlingClient(env.Client, 1)
		q := deletioncost.NewQueue(throttler)
		before := podLabelsUpdatedDelta(map[string]string{deletioncost.ResultLabel: deletioncost.ResultError})
		q.Add(pod, -7, false)
		_ = ExpectObjectReconcileFailed(ctx, env.Client, q, pod)
		after := podLabelsUpdatedDelta(map[string]string{deletioncost.ResultLabel: deletioncost.ResultError})
		Expect(after-before).To(Equal(1.0),
			"pod_labels_updated_total{result=error} should increment on a per-pod patch failure")
	})
})
