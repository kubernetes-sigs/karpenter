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

package lifecycle_test

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Launch", func() {
	var nodePool *v1beta1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	It("should launch an instance when a new NodeClaim is created", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
		Expect(cloudProvider.CreatedNodeClaims).To(HaveLen(1))
		_, err := cloudProvider.Get(ctx, nodeClaim.Status.ProviderID)
		Expect(err).ToNot(HaveOccurred())
	})
	It("should add the Launched status condition after creating the NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.Launched).Status).To(Equal(metav1.ConditionTrue))
	})
	It("should delete the nodeclaim if InsufficientCapacity is returned from the cloudprovider", func() {
		cloudProvider.NextCreateErr = cloudprovider.NewInsufficientCapacityError(fmt.Errorf("all instance types were unavailable"))
		nodeClaim := test.NodeClaim()
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
	It("should requeue with no error if NodeClassNotReady is returned from the cloudprovider", func() {
		cloudProvider.NextCreateErr = cloudprovider.NewNodeClassNotReadyError(fmt.Errorf("nodeClass isn't ready"))
		nodeClaim := test.NodeClaim()
		ExpectApplied(ctx, env.Client, nodeClaim)
		res := ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		Expect(res.Requeue).To(BeTrue())

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.Launched).Status).To(Equal(metav1.ConditionFalse))
	})
})
