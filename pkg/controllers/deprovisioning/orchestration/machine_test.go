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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning/orchestration"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
	"github.com/aws/karpenter-core/pkg/utils/nodeclaim"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var machine1, machine2, replacementMachine *v1alpha5.Machine
var provisioner *v1alpha5.Provisioner

var _ = Describe("Machine/Queue", func() {
	BeforeEach(func() {
		provisioner = test.Provisioner()
		machine1, node1 = test.MachineAndNode()
		machine2, node2 = test.MachineAndNode()
		node1.Spec.Unschedulable = true
		node2.Spec.Unschedulable = true

		ncKey = nodeclaim.Key{
			Name:      test.RandomName(),
			IsMachine: true,
		}
		replacements = []nodeclaim.Key{ncKey}
		replacementMachine, replacementNode = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: ncKey.Name,
			},
		})
	})
	Context("Queue Add", func() {
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

	Context("Queue Handle", func() {
		It("should not return an error when handling commands before the timeout", func() {
			ExpectApplied(ctx, env.Client, machine1, node1, provisioner)
			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})
			stateNode := ExpectStateNodeExistsForMachine(cluster, machine1)
			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", fakeClock.Now())
			_, err := queue.Handle(ctx, cmd)
			Expect(err).To(BeNil())
		})
		It("should return an error and clean up when a command times out", func() {
			ExpectApplied(ctx, env.Client, machine1, node1, provisioner)
			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})
			stateNode := ExpectStateNodeExistsForMachine(cluster, machine1)
			cluster.MarkForDeletion(stateNode.ProviderID())

			timeNow := fakeClock.Now()
			fakeClock.Step(1 * time.Hour)

			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", timeNow)
			requeue, err := queue.Handle(ctx, cmd)
			Expect(requeue).To(BeFalse())
			Expect(err).ToNot(BeNil())
		})
		It("should fully handle a command when replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, machine1, node1, replacementMachine, replacementNode, provisioner)
			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})
			stateNode := ExpectStateNodeExistsForMachine(cluster, machine1)
			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", fakeClock.Now())

			requeue, err := queue.Handle(ctx, cmd)
			Expect(requeue).To(BeTrue())
			Expect(err).To(BeNil())

			requeue, err = queue.Handle(ctx, cmd)
			Expect(requeue).To(BeTrue())
			Expect(err).To(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(deprovisioningevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())

			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{replacementNode}, []*v1alpha5.Machine{replacementMachine})

			requeue, err = queue.Handle(ctx, cmd)
			Expect(requeue).To(BeFalse())
			Expect(err).To(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeTrue())

			ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)
			// And expect the machine and node to be deleted
			ExpectNotFound(ctx, env.Client, machine1, node1)
		})
		It("should only finish a command when all replacements are initialized", func() {
			ncKey2 := nodeclaim.Key{
				Name:      test.RandomName(),
				IsMachine: true,
			}
			replacements = []nodeclaim.Key{ncKey, ncKey2}
			replacementMachine2, replacementNode2 := test.MachineAndNode(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name: ncKey2.Name,
				},
			})

			ExpectApplied(ctx, env.Client, machine1, node1, replacementMachine, replacementNode, replacementMachine2, replacementNode2, provisioner)
			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})
			stateNode := ExpectStateNodeExistsForMachine(cluster, machine1)

			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "", fakeClock.Now())

			requeue, err := queue.Handle(ctx, cmd)
			Expect(requeue).To(BeTrue())
			Expect(err).To(BeNil())

			requeue, err = queue.Handle(ctx, cmd)
			Expect(requeue).To(BeTrue())
			Expect(err).To(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(deprovisioningevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())
			Expect(cmd.ReplacementKeys[1].Initialized).To(BeFalse())

			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{replacementNode}, []*v1alpha5.Machine{replacementMachine})

			requeue, err = queue.Handle(ctx, cmd)
			Expect(requeue).To(BeTrue())
			Expect(err).To(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeTrue())
			Expect(cmd.ReplacementKeys[1].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(deprovisioningevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())

			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{replacementNode2}, []*v1alpha5.Machine{replacementMachine2})

			requeue, err = queue.Handle(ctx, cmd)
			Expect(requeue).To(BeFalse())
			Expect(err).To(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeTrue())
			Expect(cmd.ReplacementKeys[1].Initialized).To(BeTrue())

			ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)
			// And expect the machine and node to be deleted
			ExpectNotFound(ctx, env.Client, machine1, node1)
		})
		It("should not wait for replacments when none are needed", func() {
			ExpectApplied(ctx, env.Client, machine1, node1, replacementMachine, replacementNode, provisioner)
			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})
			stateNode := ExpectStateNodeExistsForMachine(cluster, machine1)
			cmd := orchestration.NewCommand([]nodeclaim.Key{}, []*state.StateNode{stateNode}, "", fakeClock.Now())

			requeue, err := queue.Handle(ctx, cmd)
			Expect(requeue).To(BeFalse())
			Expect(err).To(BeNil())

			ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)
			// And expect the machine and node to be deleted
			ExpectNotFound(ctx, env.Client, machine1, node1)
		})
	})

	Context("Queue Events", func() {
		It("should emit readiness events", func() {
			ExpectApplied(ctx, env.Client, machine1, node1, provisioner)
			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})
			stateNode := ExpectStateNodeExistsForMachine(cluster, machine1)
			cmd := orchestration.NewCommand(replacements, []*state.StateNode{stateNode}, "consolidation-test", fakeClock.Now())

			requeue, err := queue.Handle(ctx, cmd)
			Expect(requeue).To(BeTrue())
			Expect(err).To(BeNil())

			ExpectApplied(ctx, env.Client, replacementMachine, replacementNode)

			requeue, err = queue.Handle(ctx, cmd)
			Expect(requeue).To(BeTrue())
			Expect(err).To(BeNil())
			Expect(cmd.ReplacementKeys[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(deprovisioningevents.Launching(stateNode.NodeClaim, "consolidation-test").Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(deprovisioningevents.WaitingOnReadiness(stateNode.NodeClaim).Message)).To(BeTrue())
		})
		It("should emit termination events", func() {
			ExpectApplied(ctx, env.Client, machine1, node1, replacementMachine, replacementNode, provisioner)
			ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})
			stateNode := ExpectStateNodeExistsForMachine(cluster, machine1)
			cmd := orchestration.NewCommand([]nodeclaim.Key{}, []*state.StateNode{stateNode}, "consolidation-test", fakeClock.Now())

			requeue, err := queue.Handle(ctx, cmd)
			Expect(requeue).To(BeFalse())
			Expect(err).To(BeNil())
			terminatingEvents := deprovisioningevents.Terminating(stateNode.Node, stateNode.NodeClaim, "consolidation-test")
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)
			// And expect the machine and node to be deleted
			ExpectNotFound(ctx, env.Client, machine1, node1)
		})
	})
})
