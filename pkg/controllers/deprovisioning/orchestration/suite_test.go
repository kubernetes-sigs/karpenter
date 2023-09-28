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

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning/orchestration"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
	"github.com/aws/karpenter-core/pkg/utils/nodeclaim"
	v1 "k8s.io/api/core/v1"
	clock "k8s.io/utils/clock/testing"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var deprovisioningController *deprovisioning.Controller
var prov *provisioning.Provisioner
var cloudProvider *fake.CloudProvider
var nodeStateController controller.Controller
var machineStateController controller.Controller
var nodeClaimStateController controller.Controller
var fakeClock *clock.FakeClock
var recorder *test.EventRecorder
var queue *orchestration.Queue

var cmd orchestration.Command
var replacements []nodeclaim.Key
var candidates []*state.StateNode
var node1, node2 *v1.Node
var machine1, machine2 *v1alpha5.Machine
var provisioner *v1alpha5.Provisioner

var onDemandInstances []*cloudprovider.InstanceType
var leastExpensiveInstance, mostExpensiveInstance *cloudprovider.InstanceType
var leastExpensiveOffering, mostExpensiveOffering cloudprovider.Offering

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
	machineStateController = informer.NewMachineController(env.Client, cluster)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cluster)
	recorder = test.NewEventRecorder()
	queue = orchestration.NewQueue(ctx, env.Client, recorder, cluster)
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

    provisioner = test.Provisioner()
    machine1, node1 = test.MachineAndNode()
    machine2, node2 = test.MachineAndNode()
    replacements = []nodeclaim.Key{
        {
            Name: test.RandomName(),
            IsMachine: true,
        },
    }
    // cmd = orchestration.NewCommand(replacements, [])
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

/*
1. CanAdd
2. Retry
3. Max Delay
4. hashing
5. metrics
6. events
7. General Queue stuff
8. wait or terminate
*/
var _ = Describe("Queue", func() {
    It("should add items into the queue", func() {
        ExpectApplied(ctx, env.Client, machine1, node1, provisioner)
        ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})
        stateNode := ExpectStateNodeExistsForMachine(cluster, machine1)
        Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", fakeClock.Now()))).To(Succeed())
    })
    It("should fail to add items into that are already in the queue", func() {
        ExpectApplied(ctx, env.Client, machine1, node1, machine2, node2, provisioner)
        ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})
        stateNode1 := ExpectStateNodeExistsForMachine(cluster, machine1)
        stateNode2 := ExpectStateNodeExistsForMachine(cluster, machine2)
        // This should succeed
        Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode1, stateNode2}, "", fakeClock.Now()))).To(Succeed())
        // Both of these should fail since the stateNodes have been added in
        Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode1}, "", fakeClock.Now()))).ToNot(Succeed())
        Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode2}, "", fakeClock.Now()))).ToNot(Succeed())
    })
    It("should fail to add items into that are already in the queue", func() {
        ExpectApplied(ctx, env.Client, machine1, node1, machine2, node2, provisioner)
        ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})
        stateNode1 := ExpectStateNodeExistsForMachine(cluster, machine1)
        stateNode2 := ExpectStateNodeExistsForMachine(cluster, machine2)
        // This should succeed
        Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode1, stateNode2}, "", fakeClock.Now()))).To(Succeed())
        // Both of these should fail since the stateNodes have been added in
        Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode1}, "", fakeClock.Now()))).ToNot(Succeed())
        Expect(queue.Add(orchestration.NewCommand(replacements, []*state.StateNode{stateNode2}, "", fakeClock.Now()))).ToNot(Succeed())
    })
})
