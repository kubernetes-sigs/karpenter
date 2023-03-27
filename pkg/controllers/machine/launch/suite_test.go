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

package launch_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/machine/launch"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var ctx context.Context
var launchController controller.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Machine")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings())

	cloudProvider = fake.NewCloudProvider()
	launchController = launch.NewController(fakeClock, env.Client, cloudProvider)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	fakeClock.SetTime(time.Now())
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
})

var _ = Describe("Launch", func() {
	var provisioner *v1alpha5.Provisioner
	BeforeEach(func() {
		provisioner = test.Provisioner()
	})
	It("should add the finalizer if it doesn't exist", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, launchController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		_, ok := lo.Find(machine.Finalizers, func(f string) bool {
			return f == v1alpha5.TerminationFinalizer
		})
		Expect(ok).To(BeTrue())
	})
	It("should launch an instance when a new Machine is created", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, launchController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)

		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
		Expect(cloudProvider.CreatedMachines).To(HaveLen(1))
		_, err := cloudProvider.Get(ctx, machine.Status.ProviderID)
		Expect(err).ToNot(HaveOccurred())
	})
	It("should add the MachineCreated status condition after creating the Machine", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, launchController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineCreated).Status).To(Equal(v1.ConditionTrue))
	})
	It("should link an instance with the karpenter.sh/linked annotation", func() {
		cloudProviderMachine := &v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelInstanceTypeStable: "small-instance-type",
					v1.LabelTopologyZone:       "test-zone-1a",
					v1.LabelTopologyRegion:     "test-zone",
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeSpot,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("10"),
					v1.ResourceMemory:           resource.MustParse("100Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("8"),
					v1.ResourceMemory:           resource.MustParse("80Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
				},
			},
		}
		cloudProvider.CreatedMachines[cloudProviderMachine.Status.ProviderID] = cloudProviderMachine
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.MachineLinkedAnnotationKey: cloudProviderMachine.Status.ProviderID,
				},
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, launchController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)

		Expect(machine.Status.ProviderID).To(Equal(cloudProviderMachine.Status.ProviderID))
		ExpectResources(machine.Status.Capacity, cloudProviderMachine.Status.Capacity)
		ExpectResources(machine.Status.Allocatable, cloudProviderMachine.Status.Allocatable)

		Expect(machine.Labels).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, "small-instance-type"))
		Expect(machine.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-1a"))
		Expect(machine.Labels).To(HaveKeyWithValue(v1.LabelTopologyRegion, "test-zone"))
		Expect(machine.Labels).To(HaveKeyWithValue(v1alpha5.LabelCapacityType, v1alpha5.CapacityTypeSpot))
	})
	It("should delete the machine if InsufficientCapacity is returned from the cloudprovider", func() {
		cloudProvider.NextCreateErr = cloudprovider.NewInsufficientCapacityError(fmt.Errorf("all instance types were unavailable"))
		machine := test.Machine()
		ExpectApplied(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, launchController, client.ObjectKeyFromObject(machine))
		ExpectNotFound(ctx, env.Client, machine)
	})
	It("should delete the Machine when the Machine hasn't created past the creation TTL", func() {
		cloudProvider.AllowedCreateCalls = 0
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
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
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileFailed(ctx, launchController, client.ObjectKeyFromObject(machine))
		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineCreated).IsTrue()).To(BeFalse())

		// If the node hasn't registered in the creation timeframe, then we de-provision the Machine
		fakeClock.Step(time.Minute * 3)
		ExpectReconcileSucceeded(ctx, launchController, client.ObjectKeyFromObject(machine))
		ExpectNotFound(ctx, env.Client, machine)
	})
})
