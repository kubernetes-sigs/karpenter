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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/resources"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/counter"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/test"
)

var nodePoolController *counter.Controller
var nodePoolInformerController *informer.NodePoolController
var nodeClaimController *informer.NodeClaimController
var nodeController *informer.NodeController
var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider
var node, node2 *corev1.Node

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Counter")
}

var _ = BeforeSuite(func() {
	cloudProvider = fake.NewCloudProvider()
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...))
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client)
	nodeClaimController = informer.NewNodeClaimController(env.Client, cluster)
	nodeController = informer.NewNodeController(env.Client, cluster)
	nodePoolInformerController = informer.NewNodePoolController(env.Client, cluster)
	nodePoolController = counter.NewController(env.Client, cluster)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var nodePool *v1.NodePool
var nodeClaim, nodeClaim2 *v1.NodeClaim
var expected corev1.ResourceList

var _ = Describe("Counter", func() {
	BeforeEach(func() {
		cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
		nodePool = test.NodePool()
		instanceType := cloudProvider.InstanceTypes[0]
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: instanceType.Name,
			}},
			Status: v1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourcePods:   resource.MustParse("256"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		})
		nodeClaim2, node2 = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: instanceType.Name,
			}},
			Status: v1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourcePods:   resource.MustParse("1000"),
					corev1.ResourceMemory: resource.MustParse("5Gi"),
				},
			},
		})
		expected = counter.BaseResources.DeepCopy()
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectObjectReconciled(ctx, env.Client, nodePoolInformerController, nodePool)
		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)
	})
	It("should set well-known resource to zero when no nodes exist in the cluster", func() {
		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))
	})
	It("should set the counter from the nodeClaim and then to the node when it initializes", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		// Don't initialize the node yet
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)
		// Inform cluster state about node and nodeClaim readiness
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		expected = resources.MergeInto(expected, nodeClaim.Status.Capacity)
		expected[corev1.ResourceName("nodes")] = resource.MustParse("1")
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))

		// Change the node capacity to be different than the nodeClaim capacity
		node.Status.Capacity = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourcePods:   resource.MustParse("512"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		// Don't initialize the node yet
		ExpectMakeNodesInitialized(ctx, env.Client, node)
		// Inform cluster state about node and nodeClaim readiness
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		expected = counter.BaseResources.DeepCopy()
		expected = resources.MergeInto(expected, node.Status.Capacity)
		expected[corev1.ResourceName("nodes")] = resource.MustParse("1")
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))
	})
	It("should increase the counter when new nodes are created", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		// Should equal both the nodeClaim and node capacity
		expected = resources.MergeInto(expected, nodeClaim.Status.Capacity)
		expected[corev1.ResourceName("nodes")] = resource.MustParse("1")
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))
		expected = counter.BaseResources.DeepCopy()
		expected = resources.MergeInto(expected, node.Status.Capacity)
		expected[corev1.ResourceName("nodes")] = resource.MustParse("1")
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))
	})
	It("should decrease the counter when an existing node is deleted", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim, node2, nodeClaim2)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimController, []*corev1.Node{node, node2}, []*v1.NodeClaim{nodeClaim, nodeClaim2})

		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		// Should equal the sums of the nodeClaims and nodes
		res := corev1.ResourceList{
			corev1.ResourceCPU:           resource.MustParse("600m"),
			corev1.ResourcePods:          resource.MustParse("1256"),
			corev1.ResourceMemory:        resource.MustParse("6Gi"),
			corev1.ResourceName("nodes"): resource.MustParse("2"),
		}
		expected = resources.MergeInto(expected, res)
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))

		ExpectDeleted(ctx, env.Client, node, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		// Should equal both the nodeClaim and node capacity
		expected = counter.BaseResources.DeepCopy()
		expected = resources.MergeInto(expected, nodeClaim2.Status.Capacity)
		expected[corev1.ResourceName("nodes")] = resource.MustParse("1")
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))
		expected = counter.BaseResources.DeepCopy()
		expected = resources.MergeInto(expected, node2.Status.Capacity)
		expected[corev1.ResourceName("nodes")] = resource.MustParse("1")
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))
	})
	It("should zero out the counter when all nodes are deleted", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		// Should equal both the nodeClaim and node capacity
		expected = resources.MergeInto(expected, nodeClaim.Status.Capacity)
		expected[corev1.ResourceName("nodes")] = resource.MustParse("1")
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))
		expected = counter.BaseResources.DeepCopy()
		expected = resources.MergeInto(expected, node.Status.Capacity)
		expected[corev1.ResourceName("nodes")] = resource.MustParse("1")
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))

		ExpectDeleted(ctx, env.Client, node, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		expected = counter.BaseResources.DeepCopy()
		Expect(nodePool.Status.Resources).To(BeComparableTo(expected))
	})
})
