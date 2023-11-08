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
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
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
	"k8s.io/apimachinery/pkg/types"
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
var instanceTypes []*cloudprovider.InstanceType

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
	instanceTypes = cloudProvider.InstanceTypes
	cluster.MarkUnconsolidated()
	queue.Reset()
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("Queue", func() {
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim1, node1 = test.NodeClaimAndNode()
		fakeTopology = lo.Must(scheduling.NewTopology(ctx, env.Client, cluster, map[string]sets.Set[string]{}, []*v1.Pod{}))
		schedulingReplacementNodeClaim = scheduling.NewNodeClaim(scheduling.NewNodeClaimTemplate(nodePool), fakeTopology,
			nil, instanceTypes)
		schedulingReplacementNodeClaim.FinalizeScheduling()
	})

	Context("Add", func() {
		It("should remove the karpenter.sh/disruption taint for nodes that fail to disrupt", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			cloudProvider.AllowedCreateCalls = 0
			stateNode := ExpectStateNodeExists(cluster, node1)
			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test", 0)).ToNot(BeNil())

			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
		})
		It("should taint nodes when replacements launch successfully", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			stateNode := ExpectStateNodeExists(cluster, node1)
			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test", 0)).To(BeNil())

			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).To(ContainElement(v1beta1.DisruptionNoScheduleTaint))

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

			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test", 0)).To(BeNil())
			Expect(queue.Len()).To(Equal(1))
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		})
		It("should return an error and clean up when a command times out", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test", 0)).To(BeNil())
			Expect(queue.Len()).To(Equal(1))
			Expect(stateNode.MarkedForDeletion()).To(BeTrue())

			fakeClock.Step(1 * time.Hour)
			ExpectReconcileFailed(ctx, queue, types.NamespacedName{})
		})
		It("should fully handle a command when replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test", 0)).To(BeNil())
			Expect(queue.Len()).To(Equal(1))
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Get the command
			cmd := queue.CandidateProviderIDToCommand[nodeClaim1.Status.ProviderID]
			Expect(cmd).ToNot(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())

			ncs := ExpectNodeClaims(ctx, env.Client)
			replacementNodeClaim, _ := lo.Find(ncs, func(nc *v1beta1.NodeClaim) bool {
				return nc.Name != nodeClaim1.Name
			})
			Expect(recorder.DetectedEvent(disruptionevents.Launching(replacementNodeClaim, "test").Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(replacementNodeClaim).Message)).To(BeTrue())

			var wg sync.WaitGroup
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
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

			schedulingReplacementNodeClaim2 := scheduling.NewNodeClaim(scheduling.NewNodeClaimTemplate(nodePool), fakeTopology, nil, instanceTypes)
			schedulingReplacementNodeClaim2.FinalizeScheduling()
			Expect(queue.Add(ctx, []*state.StateNode{stateNode},
				[]*scheduling.NodeClaim{schedulingReplacementNodeClaim, schedulingReplacementNodeClaim2}, "test", 0)).To(BeNil())
			Expect(queue.Len()).To(Equal(1))

			// Get the command
			cmd := queue.CandidateProviderIDToCommand[nodeClaim1.Status.ProviderID]
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
			Expect(recorder.DetectedEvent(disruptionevents.Launching(replacementNodeClaims[0], "test").Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.Launching(replacementNodeClaims[1], "test").Message)).To(BeTrue())

			// Make one 1 node claim ready
			var wg sync.WaitGroup
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			wg.Wait()

			// Process the command
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Get NodeClaims to check events
			uninitialized := lo.Filter(cmd.ReplacementKeys, func(nck *orchestration.NodeClaimKey, _ int) bool {
				return !nck.Initialized
			})
			Expect(len(uninitialized)).To(Equal(1))
			ncs = ExpectNodeClaims(ctx, env.Client)
			replacementNodeClaims = lo.Filter(ncs, func(nc *v1beta1.NodeClaim, _ int) bool {
				return nc.Name == uninitialized[0].Name
			})
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(replacementNodeClaims[0]).Message)).To(BeTrue())

			// Make one 1 node claim ready
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			wg.Wait()

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
			Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{}, "test", 0)).To(BeNil())
			Expect(queue.Len()).To(Equal(1))

			// Get the command and process it
			cmd := queue.CandidateProviderIDToCommand[nodeClaim1.Status.ProviderID]
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

// Context("Queue Events", func() {
// 	It("should emit readiness events", func() {
// 		ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
// 		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
// 		stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
// 		Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test", 0)).ToNot(BeNil())
// 		fakeClock.Step(10 * time.Second)
// 		Expect(queue.Len()).To(Equal(1))

// 		// Get the command
// 		cmd := queue.CandidateProviderIDToCommand[nodeClaim1.Status.ProviderID]
// 		Expect(cmd).ToNot(BeNil())

// 		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
// 		Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())
// 		Expect(recorder.DetectedEvent(disruptionevents.Launching(stateNode.NodeClaim, "test").Message)).To(BeTrue())
// 		Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())
// 	})
// 	It("should emit termination events", func() {
// 		ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
// 		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
// 		stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
// 		Expect(queue.Add(ctx, []*state.StateNode{stateNode}, []*scheduling.NodeClaim{schedulingReplacementNodeClaim}, "test", 0)).ToNot(BeNil())
// 		fakeClock.Step(10 * time.Second)
// 		Expect(queue.Len()).To(Equal(1))

// 		// Get the command
// 		cmd := queue.CandidateProviderIDToCommand[nodeClaim1.Status.ProviderID]
// 		Expect(cmd).ToNot(BeNil())

// 		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
// 		terminatingEvents := disruptionevents.Terminating(stateNode.Node, stateNode.NodeClaim, "test")
// 		Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
// 		Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

// 		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
// 		// And expect the nodeClaim and node to be deleted
// 		ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
// 	})
// })
