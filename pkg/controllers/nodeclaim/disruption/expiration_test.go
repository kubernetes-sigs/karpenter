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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Expiration", func() {
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var node *v1.Node
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1beta1.NodePoolLabelKey: nodePool.Name},
			},
		})
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_disrupted metric when expired", func() {
			nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(time.Second * 30)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

			// step forward to make the node expired
			fakeClock.Step(60 * time.Second)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Expired).IsTrue()).To(BeTrue())

			metric, found := FindMetricWithLabelValues("karpenter_nodeclaims_disrupted", map[string]string{
				"type":     "expiration",
				"nodepool": nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetCounter().GetValue()).To(BeNumerically("==", 1))
		})
	})
	It("should remove the status condition from the NodeClaims when expiration is disabled", func() {
		nodePool.Spec.Disruption.ExpireAfter.Duration = nil
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Expired)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Expired)).To(BeNil())
	})
	It("should mark NodeClaims as expired", func() {
		nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(time.Second * 30)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Expired).IsTrue()).To(BeTrue())
	})
	It("should remove the status condition from non-expired NodeClaims", func() {
		nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(time.Second * 200)
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Expired)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Expired)).To(BeNil())
	})
	It("should mark NodeClaims as expired if the nodeClaim is expired but the node isn't", func() {
		nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(time.Second * 30)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectApplied(ctx, env.Client, node) // node shouldn't be expired, but nodeClaim will be
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Expired).IsTrue()).To(BeTrue())
	})
	It("should return the requeue interval for the time between now and when the nodeClaim expires", func() {
		nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(time.Second * 200)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

		fakeClock.SetTime(nodeClaim.CreationTimestamp.Time.Add(time.Second * 100))

		result := ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
		Expect(result.RequeueAfter).To(BeNumerically("~", time.Second*100, time.Second))
	})
})
