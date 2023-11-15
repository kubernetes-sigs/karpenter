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

package orchestration_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	disruptionevents "github.com/aws/karpenter-core/pkg/controllers/disruption/events"
	"github.com/aws/karpenter-core/pkg/controllers/disruption/orchestration"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	v1 "k8s.io/api/core/v1"
	clock "k8s.io/utils/clock/testing"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var cloudProvider *fake.CloudProvider
var nodeStateController controller.Controller
var nodeClaimStateController controller.Controller
var fakeClock *clock.FakeClock
var recorder *test.EventRecorder
var queue *orchestration.Queue
var prov *provisioning.Provisioner

var replacements []string
var ncName string

var nodeClaim1, nodeClaim2, replacementNodeClaim *v1beta1.NodeClaim
var nodePool *v1beta1.NodePool
var node1, node2, replacementNode *v1.Node

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption/Orchestration")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: true}))
	fakeClock = clock.NewFakeClock(time.Now())
	cloudProvider = fake.NewCloudProvider()
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cluster)
	recorder = test.NewEventRecorder()
	prov = provisioning.NewProvisioner(env.Client, env.KubernetesInterface.CoreV1(), recorder, cloudProvider, cluster)
	queue = orchestration.NewTestingQueue(env.Client, recorder, cluster, fakeClock, prov)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	recorder.Reset() // Reset the events that we captured during the run

	fakeClock.SetTime(time.Now())
	cluster.Reset()
	cloudProvider.Reset()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	cluster.MarkUnconsolidated()
	queue.Reset()
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("Queue", func() {
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim1, node1 = test.NodeClaimAndNode(
			v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   cloudProvider.InstanceTypes[0].Name,
						v1beta1.CapacityTypeLabelKey: cloudProvider.InstanceTypes[0].Offerings.Cheapest().CapacityType,
						v1.LabelTopologyZone:         cloudProvider.InstanceTypes[0].Offerings.Cheapest().Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
				},
			},
		)
		nodeClaim2, node2 = test.NodeClaimAndNode(
			v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   cloudProvider.InstanceTypes[0].Name,
						v1beta1.CapacityTypeLabelKey: cloudProvider.InstanceTypes[0].Offerings.Cheapest().CapacityType,
						v1.LabelTopologyZone:         cloudProvider.InstanceTypes[0].Offerings.Cheapest().Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
				},
			},
		)
		node1.Spec.Taints = append(node1.Spec.Taints, v1beta1.DisruptionNoScheduleTaint)
		node2.Spec.Taints = append(node2.Spec.Taints, v1beta1.DisruptionNoScheduleTaint)

		ncName = test.RandomName()
		replacements = []string{ncName}
		replacementNodeClaim, replacementNode = test.NodeClaimAndNode(
			v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: ncName,
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   cloudProvider.InstanceTypes[0].Name,
						v1beta1.CapacityTypeLabelKey: cloudProvider.InstanceTypes[0].Offerings.Cheapest().CapacityType,
						v1.LabelTopologyZone:         cloudProvider.InstanceTypes[0].Offerings.Cheapest().Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
				},
			},
		)
	})
	Context("Reconcile", func() {
		It("should keep nodes tainted when replacements haven't finished initialization", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool, replacementNodeClaim, replacementNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			stateNode := ExpectStateNodeExists(cluster, node1)
			Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "test-method", "fake-type"))).To(BeNil())

			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).To(ContainElement(v1beta1.DisruptionNoScheduleTaint))

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Update state
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).To(ContainElement(v1beta1.DisruptionNoScheduleTaint))
		})
		It("should not return an error when handling commands before the timeout", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool, replacementNodeClaim)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "test-method", "fake-type"))).To(BeNil())
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		})
		It("should untaint nodes when a command times out", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "test-method", "fake-type"))).To(BeNil())

			// Step the clock to trigger the timeout.
			fakeClock.Step(11 * time.Minute)

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
		})
		It("should fully handle a command when replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool, replacementNodeClaim, replacementNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "test-method", "fake-type")
			Expect(queue.Add(cmd)).To(BeNil())
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Get the command
			Expect(cmd.Replacements[0].Initialized).To(BeFalse())

			Expect(recorder.DetectedEvent(disruptionevents.Launching(replacementNodeClaim, cmd.Reason()).Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(replacementNodeClaim).Message)).To(BeTrue())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*v1.Node{replacementNode}, []*v1beta1.NodeClaim{replacementNodeClaim})

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())

			terminatingEvents := disruptionevents.Terminating(node1, nodeClaim1, cmd.Reason())
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should only finish a command when all replacements are initialized", func() {
			ncName2 := test.RandomName()
			replacements = []string{ncName, ncName2}
			replacementNodeClaim2, replacementNode2 := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: ncName2,
				},
			})

			ExpectApplied(ctx, env.Client, nodeClaim1, node1, replacementNodeClaim, replacementNode, replacementNodeClaim2, replacementNode2, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "test-method", "fake-type")
			Expect(queue.Add(cmd)).To(BeNil())

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim1).Message)).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeFalse())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{replacementNode}, []*v1beta1.NodeClaim{replacementNodeClaim})

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim1).Message)).To(BeTrue())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{replacementNode2}, []*v1beta1.NodeClaim{replacementNodeClaim2})

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should not wait for replacements when none are needed", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			cmd := orchestration.NewCommand([]string{}, []*state.StateNode{stateNode}, "test-method", "fake-type")
			Expect(queue.Add(cmd)).To(BeNil())

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			terminatingEvents := disruptionevents.Terminating(node1, nodeClaim1, cmd.Reason())
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should finish two commands in order as replacements are intialized", func() {
			ncName2 := test.RandomName()
			replacements2 := []string{ncName2}
			replacementnodeClaim2, replacementNode2 := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: ncName2,
				},
			})

			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodeClaim2, node2, replacementNodeClaim, replacementNode, replacementnodeClaim2, replacementNode2, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			stateNode2 := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim2)

			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "test-method", "fake-type")
			Expect(queue.Add(cmd)).To(BeNil())
			cmd2 := orchestration.NewCommand(replacements2, []*state.StateNode{stateNode2}, "test-method", "fake-type")
			Expect(queue.Add(cmd2)).To(BeNil())

			// Reconcile the first command and expect nothing to be initialized
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim1).Message)).To(BeTrue())
			Expect(cmd2.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim2).Message)).To(BeTrue())

			// Make the first command's node initialized
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{replacementNode}, []*v1beta1.NodeClaim{replacementNodeClaim})
			// Reconcile the second command and expect nothing to be initialized
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim1).Message)).To(BeTrue())
			Expect(cmd2.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim2).Message)).To(BeTrue())

			// Reconcile the first command and expect the replacement to be initialized
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd2.Replacements[0].Initialized).To(BeFalse())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)

			// Make the second command's node initialized
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{replacementNode2}, []*v1beta1.NodeClaim{replacementnodeClaim2})

			// Reconcile the second command and expect the replacement to be initialized
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd2.Replacements[0].Initialized).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2)
		})

	})
})
