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

package disruption_test

import (
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Expiration", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name},
			},
			Spec: v1.NodeClaimSpec{
				ExpireAfter: v1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)},
			},
		})
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_disrupted metric when expired", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

			// step forward to make the node expired
			fakeClock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)

			metric, found := FindMetricWithLabelValues("karpenter_nodeclaims_disrupted", map[string]string{
				"type":     "expiration",
				"nodepool": nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetCounter().GetValue()).To(BeNumerically("==", 1))
		})
		It("should fire a karpenter_nodeclaims_terminated metric when expired", func() {
			nodeClaim.Labels[v1.CapacityTypeLabelKey] = v1.CapacityTypeSpot
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

			// step forward to make the node expired
			fakeClock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)
			metric, found := FindMetricWithLabelValues("karpenter_nodeclaims_terminated", map[string]string{
				"reason":        "expiration",
				"nodepool":      nodePool.Name,
				"capacity_type": nodeClaim.Labels[v1.CapacityTypeLabelKey],
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetCounter().GetValue()).To(BeNumerically("==", 1))
		})
	})
	It("should not remove the NodeClaims when expiration is disabled", func() {
		nodeClaim.Spec.ExpireAfter.Duration = nil
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
	})
	It("should remove nodeclaims that are expired", func() {
		nodeClaim.Spec.ExpireAfter.Duration = lo.ToPtr(time.Second * 30)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		// with forceful termination, when we see a nodeclaim meets the conditions for expiration
		// we should remove it
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
	It("should not remove non-expired NodeClaims", func() {
		nodeClaim.Spec.ExpireAfter.Duration = lo.ToPtr(time.Second * 200)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
	})
	It("should delete NodeClaims if the nodeClaim is expired but the node isn't", func() {
		nodeClaim.Spec.ExpireAfter.Duration = lo.ToPtr(time.Second * 30)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectApplied(ctx, env.Client, node) // node shouldn't be expired, but nodeClaim will be
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
	It("should return the requeue interval for the time between now and when the nodeClaim expires", func() {
		nodeClaim.Spec.ExpireAfter.Duration = lo.ToPtr(time.Second * 200)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

		fakeClock.SetTime(nodeClaim.CreationTimestamp.Time.Add(time.Second * 100))

		result := ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		Expect(result.RequeueAfter).To(BeNumerically("~", time.Second*100, time.Second))
	})
})
