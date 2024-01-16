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

package counter_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/counter"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	"sigs.k8s.io/karpenter/pkg/test"
)

var nodePoolController controller.Controller
var nodePoolInformerController controller.Controller
var nodeClaimController controller.Controller
var nodeController controller.Controller
var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider
var node, node2 *v1.Node

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Counter")
}

var _ = BeforeSuite(func() {
	cloudProvider = fake.NewCloudProvider()
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeClaimController = informer.NewNodeClaimController(env.Client, cluster)
	nodeController = informer.NewNodeController(env.Client, cluster)
	nodePoolInformerController = informer.NewNodePoolController(env.Client, cluster)
	nodePoolController = counter.NewController(env.Client, cluster)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var nodePool *v1beta1.NodePool
var nodeClaim, nodeClaim2 *v1beta1.NodeClaim

var _ = Describe("Counter", func() {
	BeforeEach(func() {
		cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
		nodePool = test.NodePool()
		instanceType := cloudProvider.InstanceTypes[0]
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1beta1.NodePoolLabelKey:   nodePool.Name,
				v1.LabelInstanceTypeStable: instanceType.Name,
			}},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("100m"),
					v1.ResourceMemory:           resource.MustParse("1Gi"),
					v1.ResourcePods:             resource.MustParse("256"),
					v1.ResourceStorage:          resource.MustParse("0"),
					v1.ResourceEphemeralStorage: resource.MustParse("0"),
				},
			},
		})
		nodeClaim2, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1beta1.NodePoolLabelKey:   nodePool.Name,
				v1.LabelInstanceTypeStable: instanceType.Name,
			}},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("500m"),
					v1.ResourceMemory:           resource.MustParse("5Gi"),
					v1.ResourcePods:             resource.MustParse("1000"),
					v1.ResourceStorage:          resource.MustParse("0"),
					v1.ResourceEphemeralStorage: resource.MustParse("0"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolInformerController, client.ObjectKeyFromObject(nodePool))
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)
	})
	It("should set the counter from the nodeClaim and then to the node when it initializes", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		// Don't initialize the node yet
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)
		// Inform cluster state about node and nodeClaim readiness
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		Expect(nodePool.Status.Resources).To(BeEquivalentTo(nodeClaim.Status.Capacity))

		// Change the node capacity to be different than the nodeClaim capacity
		node.Status.Capacity = v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("1"),
			v1.ResourcePods:             resource.MustParse("512"),
			v1.ResourceMemory:           resource.MustParse("2Gi"),
			v1.ResourceStorage:          resource.MustParse("0"),
			v1.ResourceEphemeralStorage: resource.MustParse("0"),
		}
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		// Don't initialize the node yet
		ExpectMakeNodesInitialized(ctx, env.Client, node)
		// Inform cluster state about node and nodeClaim readiness
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		Expect(nodePool.Status.Resources).To(BeEquivalentTo(node.Status.Capacity))
	})
	It("should increase the counter when new nodes are created", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		// Should equal both the nodeClaim and node capacity
		Expect(nodePool.Status.Resources).To(BeEquivalentTo(nodeClaim.Status.Capacity))
		Expect(nodePool.Status.Resources).To(BeEquivalentTo(node.Status.Capacity))
	})
	It("should decrease the counter when an existing node is deleted", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim, node2, nodeClaim2)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		// Should equal the sums of the nodeClaims and nodes
		resources := v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("600m"),
			v1.ResourcePods:             resource.MustParse("1256"),
			v1.ResourceMemory:           resource.MustParse("6Gi"),
			v1.ResourceStorage:          resource.MustParse("0"),
			v1.ResourceEphemeralStorage: resource.MustParse("0"),
		}
		Expect(nodePool.Status.Resources).To(BeEquivalentTo(resources))
		Expect(nodePool.Status.Resources).To(BeEquivalentTo(resources))

		ExpectDeleted(ctx, env.Client, node, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		// Should equal both the nodeClaim and node capacity
		Expect(nodePool.Status.Resources).To(BeEquivalentTo(nodeClaim2.Status.Capacity))
		Expect(nodePool.Status.Resources).To(BeEquivalentTo(node2.Status.Capacity))
	})
	It("should zero out the counter when all nodes are deleted", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		// Should equal both the nodeClaim and node capacity
		Expect(nodePool.Status.Resources).To(BeEquivalentTo(nodeClaim.Status.Capacity))
		Expect(nodePool.Status.Resources).To(BeEquivalentTo(node.Status.Capacity))

		ExpectDeleted(ctx, env.Client, node, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		Expect(nodePool.Status.Resources).To(BeEquivalentTo(resources.ZeroResources()))
	})
})
