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
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disruptionevents "github.com/aws/karpenter-core/pkg/controllers/disruption/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/disruption/orchestration"
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

var fakeTopology *scheduling.Topology

var schedulingReplacementNodeClaim *scheduling.NodeClaim
var nodeClaim1 *v1beta1.NodeClaim
var nodePool *v1beta1.NodePool
var node1 *v1.Node

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
	queue = orchestration.NewQueue(env.Client, recorder, cluster, fakeClock, prov)
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
		fakeTopology = lo.Must(scheduling.NewTopology(ctx, env.Client, cluster, map[string]sets.Set[string]{}, []*v1.Pod{}))
		schedulingReplacementNodeClaim = scheduling.NewNodeClaim(scheduling.NewNodeClaimTemplate(nodePool), fakeTopology,
			nil, cloudProvider.InstanceTypes)
		schedulingReplacementNodeClaim.FinalizeScheduling()
	})

	Context("Add", func() {
		It("should remove the karpenter.sh/disruption taint for nodes that fail to disrupt", func() {
			nodePool.Spec.Limits = v1beta1.Limits{v1.ResourceCPU: resource.MustParse("0")}
			nodePool.Status.Resources = v1.ResourceList{v1.ResourceCPU: resource.MustParse("100")}
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			stateNode := ExpectStateNodeExists(cluster, node1)
			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test")).ToNot(BeNil())

			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
		})
		It("should taint nodes when replacements launch successfully", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			stateNode := ExpectStateNodeExists(cluster, node1)
			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test")).To(BeNil())

			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).To(ContainElement(v1beta1.DisruptionNoScheduleTaint))

			// Update state
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
			stateNode = ExpectStateNodeExists(cluster, node1)
			Expect(stateNode.MarkedForDeletion()).To(BeTrue())
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(node1.Spec.Taints).To(ContainElement(v1beta1.DisruptionNoScheduleTaint))
		})
	})
	Context("Reconcile", func() {
		It("should not return an error when handling commands before the timeout", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test")).To(BeNil())
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		})
		It("should return an error and clean up when a command times out", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test")).To(BeNil())
			stateNode = ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			Expect(stateNode.MarkedForDeletion()).To(BeTrue())

			fakeClock.Step(1 * time.Hour)
			ExpectReconcileFailed(ctx, queue, types.NamespacedName{})
		})
		It("should fully handle a command when replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test")).To(BeNil())
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Get the command
			cmd := queue.ProviderIDToCommand[nodeClaim1.Status.ProviderID]
			Expect(cmd).ToNot(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())

			ncs := ExpectNodeClaims(ctx, env.Client)
			replacementNodeClaim, ok := lo.Find(ncs, func(nc *v1beta1.NodeClaim) bool {
				return nc.Name != nodeClaim1.Name
			})
			Expect(ok).To(BeTrue())

			Expect(recorder.DetectedEvent(disruptionevents.Launching(replacementNodeClaim, "test").Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(replacementNodeClaim).Message)).To(BeTrue())

			var replacementNode *v1.Node
			replacementNodeClaim, replacementNode = ExpectNodeClaimDeployed(ctx, env.Client, cluster, cloudProvider, replacementNodeClaim)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*v1.Node{replacementNode}, []*v1beta1.NodeClaim{replacementNodeClaim})

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			cmd = queue.ProviderIDToCommand[nodeClaim1.Status.ProviderID]
			Expect(cmd).ToNot(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeTrue())

			terminatingEvents := disruptionevents.Terminating(stateNode.Node, stateNode.NodeClaim, "test")
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should only finish a command when all replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			schedulingReplacementNodeClaim2 := scheduling.NewNodeClaim(scheduling.NewNodeClaimTemplate(nodePool), fakeTopology, nil, cloudProvider.InstanceTypes)
			// Remove the hostname requirements that are used for scheduling simulation.
			schedulingReplacementNodeClaim2.FinalizeScheduling()
			Expect(queue.Add(ctx, []*state.StateNode{stateNode},
				[]*scheduling.NodeClaim{schedulingReplacementNodeClaim, schedulingReplacementNodeClaim2}, "test")).To(BeNil())

			// Get the command
			cmd := queue.ProviderIDToCommand[nodeClaim1.Status.ProviderID]
			Expect(cmd).ToNot(BeNil())

			// Process the command
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())
			Expect(cmd.ReplacementKeys[1].Initialized).To(BeFalse())

			// Get NodeClaims to check events
			ncs := ExpectNodeClaims(ctx, env.Client)
			replacementNodeClaims := lo.Filter(ncs, func(nc *v1beta1.NodeClaim, _ int) bool {
				return nc.Name != nodeClaim1.Name
			})
			Expect(len(replacementNodeClaims)).To(Equal(2))

			// Expect events to exist
			Expect(recorder.DetectedEvent(disruptionevents.Launching(replacementNodeClaims[0], "test").Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.Launching(replacementNodeClaims[1], "test").Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(replacementNodeClaims[0]).Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(replacementNodeClaims[1]).Message)).To(BeTrue())

			// initialize the first nodeclaim
			var replacementNode1, replacementNode2 *v1.Node
			var replacementNodeClaim1, replacementNodeClaim2 *v1beta1.NodeClaim

			replacementNodeClaim1, replacementNode1 = ExpectNodeClaimDeployed(ctx, env.Client, cluster, cloudProvider, replacementNodeClaims[0])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*v1.Node{replacementNode1}, []*v1beta1.NodeClaim{replacementNodeClaim1})

			// Process the command
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Check that there's one initialized nodeclaim
			initialized, ok := lo.Find(cmd.ReplacementKeys, func(nck *orchestration.NodeClaimKey) bool {
				return nck.Name == replacementNodeClaims[0].Name
			})
			Expect(ok).To(BeTrue())
			Expect(initialized.Initialized).To(BeTrue())
			// Check that there's one initialized nodeclaim
			uninitialized, ok := lo.Find(cmd.ReplacementKeys, func(nck *orchestration.NodeClaimKey) bool {
				return nck.Name == replacementNodeClaims[1].Name
			})
			Expect(ok).To(BeTrue())
			Expect(uninitialized.Initialized).To(BeFalse())

			// wait for 2nd initialization
			replacementNodeClaim2, replacementNode2 = ExpectNodeClaimDeployed(ctx, env.Client, cluster, cloudProvider, replacementNodeClaims[1])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*v1.Node{replacementNode2}, []*v1beta1.NodeClaim{replacementNodeClaim2})

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeTrue())
			Expect(cmd.ReplacementKeys[1].Initialized).To(BeTrue())

			terminatingEvents := disruptionevents.Terminating(stateNode.Node, stateNode.NodeClaim, "test")
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should not wait for replacements when none are needed", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{}, "test")).To(BeNil())

			// Get the command and process it
			cmd := queue.ProviderIDToCommand[nodeClaim1.Status.ProviderID]
			Expect(cmd).ToNot(BeNil())
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			terminatingEvents := disruptionevents.Terminating(stateNode.Node, stateNode.NodeClaim, "test")
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
	})
})

// func ExpectQueueReconcileSucceeded(ctx context.Context, queue *Queue) {
// 	ExpectQueueNotEmpty(ctx, queue)
// 	ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
// }

// func ExpectQueueReconcileFailed(ctx context.Context, queue *Queue) {
// 	ExpectQueueNotEmpty(ctx, queue)
// 	ExpectReconcileFailed(ctx, queue, types.NamespacedName{})
// }

// func ExpectQueueNotEmpty(ctx context.Context, queue *Queue) {
// 	ctx, cancel := context.WithTimeout(ctx, time.Second*10) // give up after 10s
// 	defer GinkgoRecover()
// 	defer cancel()
// 	for {
// 		select {
// 		case <-time.After(50 * time.Millisecond):
// 			if queue.Len() != 0 {
// 				return
// 			}
// 		case <-ctx.Done():
// 			Fail("waiting for command to be requeued")
// 		}
// 	}
// }
