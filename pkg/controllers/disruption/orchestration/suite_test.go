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
	. "knative.dev/pkg/logging/testing"

	disruptionevents "github.com/aws/karpenter-core/pkg/controllers/disruption/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	"github.com/aws/karpenter-core/pkg/utils/nodeclaim"

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

var replacements []nodeclaim.Key
var node1, node2, replacementNode *v1.Node
var ncKey nodeclaim.Key

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption/Orchestration")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: true}))
	fakeClock = clock.NewFakeClock(time.Now())
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
	cluster.MarkUnconsolidated()
	queue.Reset()
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

// addCommandToQueue adds the created command directly into the queue, rather than launching nodes and adding a delay.
func addCommandToQueue(cmd *orchestration.Command, q *orchestration.Queue) {
	q.RateLimitingInterface.Add(cmd)
	for _, candidate := range cmd.Candidates {
		q.CandidateProviderIDToCommand[candidate.ProviderID()] = cmd
	}
}

var nodeClaim1, nodeClaim2, replacementNodeClaim *v1beta1.NodeClaim
var nodePool *v1beta1.NodePool

var _ = Describe("Queue", func() {
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim1, node1 = test.NodeClaimAndNode()
		nodeClaim2, node2 = test.NodeClaimAndNode()
		node1.Spec.Unschedulable = true
		node2.Spec.Unschedulable = true

		ncKey = nodeclaim.Key{
			Name:      test.RandomName(),
			IsMachine: false,
		}
		replacements = []nodeclaim.Key{ncKey}
		replacementNodeClaim, replacementNode = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: ncKey.Name,
			},
		})
	})
	Context("Queue Reconcile", func() {
		It("should not return an error when handling commands before the timeout", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", fakeClock.Now())
			addCommandToQueue(cmd, queue)
			ExpectApplied(ctx, env.Client, replacementNodeClaim, replacementNode)
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		})
		It("should return an error and clean up when a command times out", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			cluster.MarkForDeletion(stateNode.ProviderID())

			timeNow := fakeClock.Now()
			fakeClock.Step(1 * time.Hour)

			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", timeNow)
			addCommandToQueue(cmd, queue)
			ExpectReconcileFailed(ctx, queue, types.NamespacedName{})
		})
		It("should fully handle a command when replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, replacementNodeClaim, replacementNode, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", fakeClock.Now())
			addCommandToQueue(cmd, queue)

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{replacementNode}, []*v1beta1.NodeClaim{replacementNodeClaim})

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should only finish a command when all replacements are initialized", func() {
			ncKey2 := nodeclaim.Key{
				Name:      test.RandomName(),
				IsMachine: false,
			}
			replacements = []nodeclaim.Key{ncKey, ncKey2}
			replacementnodeClaim2, replacementNode2 := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: ncKey2.Name,
				},
			})

			ExpectApplied(ctx, env.Client, nodeClaim1, node1, replacementNodeClaim, replacementNode, replacementnodeClaim2, replacementNode2, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", fakeClock.Now())
			addCommandToQueue(cmd, queue)

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())
			Expect(cmd.ReplacementKeys[1].Initialized).To(BeFalse())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{replacementNode}, []*v1beta1.NodeClaim{replacementNodeClaim})

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeTrue())
			Expect(cmd.ReplacementKeys[1].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{replacementNode2}, []*v1beta1.NodeClaim{replacementnodeClaim2})

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeTrue())
			Expect(cmd.ReplacementKeys[1].Initialized).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should not wait for replacements when none are needed", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, replacementNodeClaim, replacementNode, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			cmd := orchestration.NewCommand([]nodeclaim.Key{}, []*state.StateNode{stateNode}, "", fakeClock.Now())
			addCommandToQueue(cmd, queue)
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
	})

	Context("Queue Events", func() {
		It("should emit readiness events", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "consolidation-test", fakeClock.Now())
			addCommandToQueue(cmd, queue)

			ExpectApplied(ctx, env.Client, replacementNodeClaim, replacementNode)
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.Launching(stateNode.NodeClaim, "consolidation-test").Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())
		})
		It("should emit termination events", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, replacementNodeClaim, replacementNode, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			cmd := orchestration.NewCommand([]nodeclaim.Key{}, []*state.StateNode{stateNode}, "consolidation-test", fakeClock.Now())
			addCommandToQueue(cmd, queue)

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			terminatingEvents := disruptionevents.Terminating(stateNode.Node, stateNode.NodeClaim, "consolidation-test")
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
	})
})
