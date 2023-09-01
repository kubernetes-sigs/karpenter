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

package garbagecollection_test

import (
	"time"

	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/test"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("NodeClaim/GarbageCollection", func() {
	var nodePool *v1beta1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	It("should delete the NodeClaim when the Node never appears and the instance is gone", func() {
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

		// Step forward to move past the cache eventual consistency timeout
		fakeClock.SetTime(time.Now().Add(time.Second * 20))

		// Delete the nodeClaim from the cloudprovider
		Expect(cloudProvider.Delete(ctx, machineutil.NewFromNodeClaim(nodeClaim))).To(Succeed())

		// Expect the NodeClaim to be removed now that the Instance is gone
		ExpectReconcileSucceeded(ctx, garbageCollectionController, client.ObjectKey{})
		ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
	It("should delete many NodeClaims when the Node never appears and the instance is gone", func() {
		var nodeClaims []*v1beta1.NodeClaim
		for i := 0; i < 100; i++ {
			nodeClaims = append(nodeClaims, test.NodeClaim(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey: nodePool.Name,
					},
				},
			}))
		}
		ExpectApplied(ctx, env.Client, nodePool)
		workqueue.ParallelizeUntil(ctx, len(nodeClaims), len(nodeClaims), func(i int) {
			defer GinkgoRecover()
			ExpectApplied(ctx, env.Client, nodeClaims[i])
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaims[i]))
			nodeClaims[i] = ExpectExists(ctx, env.Client, nodeClaims[i])
		})

		// Step forward to move past the cache eventual consistency timeout
		fakeClock.SetTime(time.Now().Add(time.Second * 20))

		workqueue.ParallelizeUntil(ctx, len(nodeClaims), len(nodeClaims), func(i int) {
			defer GinkgoRecover()
			// Delete the NodeClaim from the cloudprovider
			Expect(cloudProvider.Delete(ctx, machineutil.NewFromNodeClaim(nodeClaims[i]))).To(Succeed())
		})

		// Expect the NodeClaims to be removed now that the Instance is gone
		ExpectReconcileSucceeded(ctx, garbageCollectionController, client.ObjectKey{})

		workqueue.ParallelizeUntil(ctx, len(nodeClaims), len(nodeClaims), func(i int) {
			defer GinkgoRecover()
			ExpectFinalizersRemoved(ctx, env.Client, nodeClaims[i])
		})
		ExpectNotFound(ctx, env.Client, lo.Map(nodeClaims, func(n *v1beta1.NodeClaim, _ int) client.Object { return n })...)
	})
	It("shouldn't delete the NodeClaim when the Node isn't there but the instance is there", func() {
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

		// Step forward to move past the cache eventual consistency timeout
		fakeClock.SetTime(time.Now().Add(time.Second * 20))

		// Reconcile the NodeClaim. It should not be deleted by this flow since it has never been registered
		ExpectReconcileSucceeded(ctx, garbageCollectionController, client.ObjectKey{})
		ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		ExpectExists(ctx, env.Client, nodeClaim)
	})
})
