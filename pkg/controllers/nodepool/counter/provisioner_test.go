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

package counter_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var provisioner *v1alpha5.Provisioner
var machine, machine2 *v1alpha5.Machine

var _ = Describe("Provisioner Counter", func() {
	BeforeEach(func() {
		cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
		provisioner = test.Provisioner()
		instanceType := cloudProvider.InstanceTypes[0]
		machine, node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       instanceType.Name,
			}},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourcePods:   resource.MustParse("256"),
					v1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		})
		machine2, node2 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       instanceType.Name,
			}},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("500m"),
					v1.ResourcePods:   resource.MustParse("1000"),
					v1.ResourceMemory: resource.MustParse("5Gi"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerInformerController, client.ObjectKeyFromObject(provisioner))
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)
	})

	It("should set the counter from the machine and then to the node when it initializes", func() {
		ExpectApplied(ctx, env.Client, node, machine)
		// Don't initialize the node yet
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)
		// Inform cluster state about node and machine readiness
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		Expect(provisioner.Status.Resources).To(BeEquivalentTo(machine.Status.Capacity))

		// Change the node capacity to be different than the machine capacity
		node.Status.Capacity = v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("1"),
			v1.ResourcePods:   resource.MustParse("512"),
			v1.ResourceMemory: resource.MustParse("2Gi"),
		}
		ExpectApplied(ctx, env.Client, node, machine)
		// Don't initialize the node yet
		ExpectMakeNodesInitialized(ctx, env.Client, node)
		// Inform cluster state about node and machine readiness
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		Expect(provisioner.Status.Resources).To(BeEquivalentTo(node.Status.Capacity))
	})

	It("should increase the counter when new nodes are created", func() {
		ExpectApplied(ctx, env.Client, node, machine)
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeController, machineController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		// Should equal both the machine and node capacity
		Expect(provisioner.Status.Resources).To(BeEquivalentTo(machine.Status.Capacity))
		Expect(provisioner.Status.Resources).To(BeEquivalentTo(node.Status.Capacity))
	})
	It("should decrease the counter when an existing node is deleted", func() {
		ExpectApplied(ctx, env.Client, node, machine, node2, machine2)
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeController, machineController, []*v1.Node{node, node2}, []*v1alpha5.Machine{machine, machine2})

		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		// Should equal the sums of the machines and nodes
		resources := v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("600m"),
			v1.ResourcePods:   resource.MustParse("1256"),
			v1.ResourceMemory: resource.MustParse("6Gi"),
		}
		Expect(provisioner.Status.Resources).To(BeEquivalentTo(resources))
		Expect(provisioner.Status.Resources).To(BeEquivalentTo(resources))

		ExpectDeleted(ctx, env.Client, node, machine)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		// Should equal both the machine and node capacity
		Expect(provisioner.Status.Resources).To(BeEquivalentTo(machine2.Status.Capacity))
		Expect(provisioner.Status.Resources).To(BeEquivalentTo(node2.Status.Capacity))
	})
	It("should nil out the counter when all nodes are deleted", func() {
		ExpectApplied(ctx, env.Client, node, machine)
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeController, machineController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		// Should equal both the machine and node capacity
		Expect(provisioner.Status.Resources).To(BeEquivalentTo(machine.Status.Capacity))
		Expect(provisioner.Status.Resources).To(BeEquivalentTo(node.Status.Capacity))

		ExpectDeleted(ctx, env.Client, node, machine)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)
		Expect(provisioner.Status.Resources).To(BeNil())
	})
})
