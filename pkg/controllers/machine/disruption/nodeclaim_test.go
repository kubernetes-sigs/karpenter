/*
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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var _ = Describe("NodeClaim/Disruption", func() {
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
	It("should set multiple disruption conditions simultaneously", func() {
		cp.Drifted = "drifted"
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenEmpty
		nodePool.Spec.Disruption.ConsolidateAfter.Duration = lo.ToPtr(time.Second * 30)
		nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(time.Second * 30)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		// step forward to make the node expired and empty
		fakeClock.Step(60 * time.Second)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeDrifted).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeEmpty).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeExpired).IsTrue()).To(BeTrue())
	})
	It("should remove multiple disruption conditions simultaneously", func() {
		nodePool.Spec.Disruption.ExpireAfter.Duration = nil
		nodePool.Spec.Disruption.ConsolidateAfter.Duration = nil

		nodeClaim.StatusConditions().MarkTrue(v1beta1.NodeDrifted)
		nodeClaim.StatusConditions().MarkTrue(v1beta1.NodeEmpty)
		nodeClaim.StatusConditions().MarkTrue(v1beta1.NodeExpired)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		// Drift, Expiration, and Emptiness are disabled through configuration
		ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeDrifted)).To(BeNil())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeEmpty)).To(BeNil())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeExpired)).To(BeNil())
	})
})
