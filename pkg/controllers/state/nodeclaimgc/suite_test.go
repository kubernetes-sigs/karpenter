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

package nodeclaimgc_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	nodeclaimgc "sigs.k8s.io/karpenter/pkg/controllers/state/nodeclaimgc"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx                 context.Context
	env                 *test.Environment
	cluster             *state.Cluster
	clusterCost         *cost.ClusterCost
	cloudProvider       *fake.CloudProvider
	gcController        *nodeclaimgc.Controller
	nodeClaimController *informer.NodeClaimController
)

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers/State/NodeClaimGC")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	ctx = options.ToContext(ctx, test.Options())
	cloudProvider = fake.NewCloudProvider()
	cluster = state.NewCluster(env.Clock, env.Client, cloudProvider)
	clusterCost = cost.NewClusterCost(ctx, cloudProvider, env.Client)
	gcController = nodeclaimgc.NewController(env.Client, cluster)
	nodeClaimController = informer.NewNodeClaimController(env.Client, cloudProvider, cluster, clusterCost)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
	cloudProvider.Reset()
})

func request(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

var _ = Describe("NodeClaim Cluster State GC", func() {
	It("should heal the wedge caused by a seed landing after the informer's delete cleanup", func() {
		// The informer observes the NotFound and cleans up first (a no-op against empty state),
		// then the provisioner's seed lands and strands name -> "". Synced() is now stuck false
		// until state.nodeclaimgc reconciles.
		nodeClaim := test.NodeClaim()
		nodeClaim.Status.ProviderID = ""

		// Informer delete cleanup runs first against a NodeClaim that doesn't exist -> no-op.
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		// The provisioner's post-create seed lands afterwards, stranding the ghost entry.
		cluster.UpdateNodeClaim(nodeClaim)
		Expect(cluster.Synced(ctx)).To(BeFalse())

		// A later re-check of the NodeClaim (still gone) heals it.
		ExpectReconciled(ctx, gcController, request(nodeClaim.Name))
		Expect(cluster.NodeClaimExists(nodeClaim.Name)).To(BeFalse())
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should not evict an unlaunched NodeClaim that still exists after the grace period", func() {
		// A NodeClaim that is genuinely still launching (empty providerID) must keep Synced() false and must not be
		// evicted. Any later deletion lands after the seed, so the informer's normal cleanup handles it.
		nodeClaim := test.NodeClaim()
		nodeClaim.Status.ProviderID = ""
		ExpectApplied(ctx, env.Client, nodeClaim)
		cluster.UpdateNodeClaim(nodeClaim)

		Expect(cluster.UnlaunchedNodeClaimExists(nodeClaim.Name)).To(BeTrue())
		Expect(cluster.Synced(ctx)).To(BeFalse())

		ExpectReconciled(ctx, gcController, request(nodeClaim.Name))

		Expect(cluster.UnlaunchedNodeClaimExists(nodeClaim.Name)).To(BeTrue())
		Expect(cluster.Synced(ctx)).To(BeFalse())
	})
	It("should not evict a launched entry even if its NodeClaim no longer exists", func() {
		// Launched entries (non-empty providerID) never hold Synced() false and are owned by the informer, not this
		// controller. Even with the NodeClaim is gone from cache, the controller must leave them untouched.
		nodeClaim := test.NodeClaim()
		nodeClaim.Status.ProviderID = test.RandomProviderID()
		cluster.UpdateNodeClaim(nodeClaim)

		Expect(cluster.NodeClaimExists(nodeClaim.Name)).To(BeTrue())

		ExpectReconciled(ctx, gcController, request(nodeClaim.Name))

		Expect(cluster.NodeClaimExists(nodeClaim.Name)).To(BeTrue())
	})
	It("should be a no-op when the entry was already cleaned up", func() {
		nodeClaim := test.NodeClaim()
		nodeClaim.Status.ProviderID = ""
		cluster.UpdateNodeClaim(nodeClaim)
		cluster.DeleteNodeClaim(nodeClaim.Name)

		Expect(cluster.NodeClaimExists(nodeClaim.Name)).To(BeFalse())

		ExpectReconciled(ctx, gcController, request(nodeClaim.Name))

		Expect(cluster.NodeClaimExists(nodeClaim.Name)).To(BeFalse())
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
})
