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

package machine_test

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
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/machine"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	ptermination "github.com/aws/karpenter-core/pkg/termination"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var ctx context.Context
var machineController controller.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider
var settingsStore test.SettingsStore

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Node")
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
	recorder := test.NewEventRecorder()
	evictionQueue := ptermination.NewEvictionQueue(ctx, env.KubernetesInterface.CoreV1(), recorder)
	machineController = machine.NewController(fakeClock, env.Client, cloudProvider, ptermination.NewTerminator(fakeClock, env.Client, cloudProvider, evictionQueue), recorder)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Controller", func() {
	BeforeEach(func() {
		settingsStore = test.SettingsStore{
			settings.ContextKey: test.Settings(),
		}
	})
	AfterEach(func() {
		fakeClock.SetTime(time.Now())
		ExpectCleanedUp(ctx, env.Client)
		cloudProvider.Reset()
	})

	Context("Finalizer", func() {
		It("should add the finalizer if it doesn't exist", func() {
			machine := test.Machine()
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)
			_, ok := lo.Find(machine.Finalizers, func(f string) bool {
				return f == v1alpha5.TerminationFinalizer
			})
			Expect(ok).To(BeTrue())
		})
	})
	Context("Launch", func() {
		It("should launch an instance when a new Machine is created", func() {
			machine := test.Machine()
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			Expect(cloudProvider.CreatedMachines).To(HaveLen(1))
			_, err := cloudProvider.Get(ctx, machine.Name, "")
			Expect(err).ToNot(HaveOccurred())
		})
		It("should get an instance and hydrate the Machine when the Machine is already created", func() {
			machine := test.Machine()
			cloudProviderMachine := &v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name: machine.Name,
					Labels: map[string]string{
						v1.LabelInstanceTypeStable: "small-instance-type",
						v1.LabelTopologyZone:       "test-zone-1a",
						v1.LabelTopologyRegion:     "test-zone",
						v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeSpot,
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: fmt.Sprintf("fake://%s", test.RandomName()),
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
			cloudProvider.CreatedMachines[machine.Name] = cloudProviderMachine
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)

			Expect(machine.Status.ProviderID).To(Equal(cloudProviderMachine.Status.ProviderID))
			ExpectResources(machine.Status.Capacity, cloudProviderMachine.Status.Capacity)
			ExpectResources(machine.Status.Allocatable, cloudProviderMachine.Status.Allocatable)

			Expect(machine.Labels).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, "small-instance-type"))
			Expect(machine.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-1a"))
			Expect(machine.Labels).To(HaveKeyWithValue(v1.LabelTopologyRegion, "test-zone"))
			Expect(machine.Labels).To(HaveKeyWithValue(v1alpha5.LabelCapacityType, v1alpha5.CapacityTypeSpot))
		})
		It("should add the MachineCreated status condition after creating the Machine", func() {
			machine := test.Machine()
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineCreated).Status).To(Equal(v1.ConditionTrue))
		})
	})
	Context("Registration", func() {

	})
	Context("Initialization", func() {

	})
	Context("Liveness", func() {

	})
	Context("Termination", func() {

	})
})
