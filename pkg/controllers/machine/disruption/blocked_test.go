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

package disruption_test

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Blocked", func() {
	var provisioner *v1alpha5.Provisioner
	var machine *v1alpha5.Machine
	var node *v1.Node
	BeforeEach(func() {
		provisioner = test.Provisioner()
		machine, node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       test.RandomName(),
					v1alpha5.LabelCapacityType:       "on-demand",
					v1.LabelTopologyZone:             "us-west-2a",
				},
				Annotations: map[string]string{
					v1alpha5.ProvisionerHashAnnotationKey: provisioner.Hash(),
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
			},
		})
	})
	It("should not detect a node as blocked", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		// Make state aware of the node and machine.
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeFalse())
	})
	// 1. Must be synced
	// 2. Node in cluster state
	// 3. Node and Machine must exist in cluster state
	// 4. Not Marked for Deletion
	// 5. Initialized
	// 6. Not Nominated for scheduling
	// 7. Has capacity type, topology zone, provisioner name label key labels
	// 8. Instance type label
	// 9. No PDB pod or do-not-evict
	It("should detect a node as blocked if cluster state is not synced", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{}, []*v1alpha5.Machine{})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
	It("should detect a node as blocked if cluster state has machine but not node", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{}, []*v1alpha5.Machine{machine})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
	It("should detect a node as blocked if cluster state has node but not machine", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
	It("should detect a node as blocked if the node is marked for deletion", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
		cluster.MarkForDeletion(node.Name)

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
	It("should detect a node as blocked if the node is not initialized", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		// Inform cluster state about node and machine readiness, but not initialize
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
	It("should detect a node as blocked if it was the target of a recent scheduling simulation", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
		cluster.NominateNodeForPod(ctx, node.Name)

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
	It("should detect a node as blocked if it does not have the correct labels", func() {
		machine.Labels = map[string]string{
			v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
			v1.LabelInstanceTypeStable:       test.RandomName(),
		}
		node.Labels = map[string]string{
			v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
			v1.LabelInstanceTypeStable:       test.RandomName(),
		}
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
		cluster.NominateNodeForPod(ctx, node.Name)

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
	It("should detect a node as blocked if it has a do-not-evict pod", func() {
		// Add a do-not-evict pod to the node so that it's not deprovisionable.
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.DoNotEvictPodAnnotationKey: "true",
				}}})

		ExpectApplied(ctx, env.Client, provisioner, machine, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
		cluster.NominateNodeForPod(ctx, node.Name)

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
	It("should detect a node as blocked if it has a pod with a blocking PDB", func() {
		// Add a do-not-evict pod to the node so that it's not deprovisionable.
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"test": "app",
				},
			}},
		)
		pdb := test.PodDisruptionBudget(test.PDBOptions{
			Labels: map[string]string{
				"test": "app",
			},
			MaxUnavailable: intstr.ValueOrDefault(nil, intstr.FromInt(0)),
		})

		ExpectApplied(ctx, env.Client, provisioner, machine, node, pod, pdb)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1beta1.NodeDeprovisioningBlocked).IsTrue()).To(BeTrue())
	})
})
