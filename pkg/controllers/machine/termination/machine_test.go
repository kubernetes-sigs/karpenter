package termination_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var _ = Describe("Machine", func() {
	var provisioner *v1alpha5.Provisioner
	var machine *v1alpha5.Machine

	BeforeEach(func() {
		provisioner = test.Provisioner()
		machine = test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
				Finalizers: []string{
					v1alpha5.TerminationFinalizer,
				},
			},
			Spec: v1alpha5.MachineSpec{
				Resources: v1alpha5.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:          resource.MustParse("2"),
						v1.ResourceMemory:       resource.MustParse("50Mi"),
						v1.ResourcePods:         resource.MustParse("5"),
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			},
		})
	})
	It("should delete the node and the CloudProvider Machine when Machine deletion is triggered", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		_, err := cloudProvider.Get(ctx, machine.Status.ProviderID)
		Expect(err).ToNot(HaveOccurred())

		node := test.MachineLinkedNode(machine)
		ExpectApplied(ctx, env.Client, node)

		// Expect the node and the machine to both be gone
		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine)) // triggers the node deletion
		ExpectFinalizersRemoved(ctx, env.Client, node)
		ExpectNotFound(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine)) // now all nodes are gone so machine deletion continues
		ExpectNotFound(ctx, env.Client, machine, node)

		// Expect the machine to be gone from the cloudprovider
		_, err = cloudProvider.Get(ctx, machine.Status.ProviderID)
		Expect(cloudprovider.IsMachineNotFoundError(err)).To(BeTrue())
	})
	It("should delete multiple Nodes if multiple Nodes map to the Machine", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		_, err := cloudProvider.Get(ctx, machine.Status.ProviderID)
		Expect(err).ToNot(HaveOccurred())

		node1 := test.MachineLinkedNode(machine)
		node2 := test.MachineLinkedNode(machine)
		node3 := test.MachineLinkedNode(machine)
		ExpectApplied(ctx, env.Client, node1, node2, node3)

		// Expect the node and the machine to both be gone
		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine)) // triggers the node deletion
		ExpectFinalizersRemoved(ctx, env.Client, node1, node2, node3)
		ExpectNotFound(ctx, env.Client, node1, node2, node3)

		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine)) // now all nodes are gone so machine deletion continues
		ExpectNotFound(ctx, env.Client, machine, node1, node2, node3)

		// Expect the machine to be gone from the cloudprovider
		_, err = cloudProvider.Get(ctx, machine.Status.ProviderID)
		Expect(cloudprovider.IsMachineNotFoundError(err)).To(BeTrue())
	})
	It("should delete the Instance if the Machine is linked but doesn't have its providerID resolved yet", func() {
		node := test.MachineLinkedNode(machine)

		machine.Annotations = lo.Assign(machine.Annotations, map[string]string{v1alpha5.MachineLinkedAnnotationKey: machine.Status.ProviderID})
		machine.Status.ProviderID = ""
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		// Expect the machine to be gone
		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine)) // triggers the machine deletion
		ExpectNotFound(ctx, env.Client, machine)

		// Expect the machine to be gone from the cloudprovider
		_, err := cloudProvider.Get(ctx, machine.Annotations[v1alpha5.MachineLinkedAnnotationKey])
		Expect(cloudprovider.IsMachineNotFoundError(err)).To(BeTrue())
	})
	It("should not delete the Machine until all the Nodes are removed", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		_, err := cloudProvider.Get(ctx, machine.Status.ProviderID)
		Expect(err).ToNot(HaveOccurred())

		node := test.MachineLinkedNode(machine)
		ExpectApplied(ctx, env.Client, node)

		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine)) // triggers the node deletion
		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine)) // the node still hasn't been deleted, so the machine should remain

		ExpectExists(ctx, env.Client, machine)
		ExpectExists(ctx, env.Client, node)

		ExpectFinalizersRemoved(ctx, env.Client, node)
		ExpectNotFound(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine)) // now the machine should be gone

		ExpectNotFound(ctx, env.Client, machine)
	})
	It("should not call Delete() on the CloudProvider if the machine hasn't been launched yet", func() {
		machine.Status.ProviderID = ""
		ExpectApplied(ctx, env.Client, provisioner, machine)

		// Expect the machine to be gone
		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine))

		Expect(cloudProvider.DeleteCalls).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine)
	})
	It("should not delete nodes without provider ids if the machine hasn't been launched yet", func() {
		// Generate 10 nodes, none of which have a provider id
		var nodes []*v1.Node
		for i := 0; i < 10; i++ {
			nodes = append(nodes, test.Node())
		}
		ExpectApplied(ctx, env.Client, lo.Map(nodes, func(n *v1.Node, _ int) client.Object { return n })...)

		ExpectApplied(ctx, env.Client, provisioner, machine)

		// Expect the machine to be gone
		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, machineTerminationController, client.ObjectKeyFromObject(machine))

		ExpectNotFound(ctx, env.Client, machine)
		for _, node := range nodes {
			ExpectExists(ctx, env.Client, node)
		}
	})
})
