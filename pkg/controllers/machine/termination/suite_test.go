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

package termination_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	machinelifecycle "github.com/aws/karpenter-core/pkg/controllers/machine/lifecycle"
	machinetermination "github.com/aws/karpenter-core/pkg/controllers/machine/termination"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var ctx context.Context
var env *test.Environment
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider
var machineController controller.Controller
var terminationController controller.Controller

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Machine")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1.Node{}, "spec.providerID", func(obj client.Object) []string {
			return []string{obj.(*v1.Node).Spec.ProviderID}
		})
	}))
	ctx = settings.ToContext(ctx, test.Settings())
	cloudProvider = fake.NewCloudProvider()
	machineController = machinelifecycle.NewController(fakeClock, env.Client, cloudProvider, events.NewRecorder(&record.FakeRecorder{}))
	terminationController = machinetermination.NewController(env.Client, cloudProvider)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	fakeClock.SetTime(time.Now())
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
})

var _ = Describe("Termination", func() {
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
		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // triggers the node deletion
		ExpectFinalizersRemoved(ctx, env.Client, node)
		ExpectNotFound(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // now all nodes are gone so machine deletion continues
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
		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // triggers the node deletion
		ExpectFinalizersRemoved(ctx, env.Client, node1, node2, node3)
		ExpectNotFound(ctx, env.Client, node1, node2, node3)

		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // now all nodes are gone so machine deletion continues
		ExpectNotFound(ctx, env.Client, machine, node1, node2, node3)

		// Expect the machine to be gone from the cloudprovider
		_, err = cloudProvider.Get(ctx, machine.Status.ProviderID)
		Expect(cloudprovider.IsMachineNotFoundError(err)).To(BeTrue())
	})
	It("should delete the Node if the Machine is linked but doesn't have its providerID resolved yet", func() {
		node := test.MachineLinkedNode(machine)

		machine.Annotations = lo.Assign(machine.Annotations, map[string]string{v1alpha5.MachineLinkedAnnotationKey: machine.Status.ProviderID})
		machine.Status.ProviderID = ""
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		// Expect the machine to be gone
		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // triggers the node deletion
		ExpectFinalizersRemoved(ctx, env.Client, node)
		ExpectNotFound(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // now all nodes are gone so machine deletion continues
		ExpectNotFound(ctx, env.Client, machine, node)

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
		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // triggers the node deletion
		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // the node still hasn't been deleted, so the machine should remain

		ExpectExists(ctx, env.Client, machine)
		ExpectExists(ctx, env.Client, node)

		ExpectFinalizersRemoved(ctx, env.Client, node)
		ExpectNotFound(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine)) // now the machine should be gone

		ExpectNotFound(ctx, env.Client, machine)
	})
	It("should not call Delete() on the CloudProvider if the machine hasn't been launched yet", func() {
		machine.Status.ProviderID = ""
		ExpectApplied(ctx, env.Client, provisioner, machine)

		// Expect the machine to be gone
		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine))

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
		ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(machine))

		ExpectNotFound(ctx, env.Client, machine)
		for _, node := range nodes {
			ExpectExists(ctx, env.Client, node)
		}
	})
})
