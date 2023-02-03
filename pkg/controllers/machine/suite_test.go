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
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/machine"
	"github.com/aws/karpenter-core/pkg/controllers/machine/terminator"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var ctx context.Context
var machineController controller.Controller
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
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1.Node{}, "spec.providerID", func(obj client.Object) []string {
			return []string{obj.(*v1.Node).Spec.ProviderID}
		})
	}))
	ctx = settings.ToContext(ctx, test.Settings())

	cloudProvider = fake.NewCloudProvider()
	terminator := terminator.NewTerminator(fakeClock, env.Client, cloudProvider, terminator.NewEvictionQueue(ctx, env.KubernetesInterface.CoreV1(), events.NewRecorder(&record.FakeRecorder{})))
	machineController = machine.NewController(fakeClock, env.Client, cloudProvider, terminator, events.NewRecorder(&record.FakeRecorder{}))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Controller", func() {
	BeforeEach(func() {
		ctx = settings.ToContext(ctx, test.Settings())
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
		It("should match the Machine to the Node when the Node comes online", func() {
			machine := test.Machine()
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
		})
		It("should add the owner reference to the Node when the Node comes online", func() {
			machine := test.Machine()
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			node = ExpectExists(ctx, env.Client, node)
			ExpectOwnerReferenceExists(node, machine)
		})
		It("should sync the labels to the Node when the Node comes online", func() {
			machine := test.Machine(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"custom-label":       "custom-value",
						"other-custom-label": "other-custom-value",
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(machine.Labels).To(HaveKeyWithValue("custom-label", "custom-value"))
			Expect(machine.Labels).To(HaveKeyWithValue("other-custom-label", "other-custom-value"))

			node := test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			node = ExpectExists(ctx, env.Client, node)

			// Expect Node to have all the labels that the Machine has
			for k, v := range machine.Labels {
				Expect(node.Labels).To(HaveKeyWithValue(k, v))
			}
		})
		It("should sync the annotations to the Node when the Node comes online", func() {
			machine := test.Machine(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha5.DoNotConsolidateNodeAnnotationKey: "true",
						"my-custom-annotation":                     "my-custom-value",
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(machine.Annotations).To(HaveKeyWithValue(v1alpha5.DoNotConsolidateNodeAnnotationKey, "true"))
			Expect(machine.Annotations).To(HaveKeyWithValue("my-custom-annotation", "my-custom-value"))

			node := test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			node = ExpectExists(ctx, env.Client, node)

			// Expect Node to have all the annotations that the Machine has
			for k, v := range machine.Annotations {
				Expect(node.Annotations).To(HaveKeyWithValue(k, v))
			}
		})
		It("should sync the taints to the Node when the Node comes online", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Taints: []v1.Taint{
						{
							Key:    "custom-taint",
							Effect: v1.TaintEffectNoSchedule,
							Value:  "custom-value",
						},
						{
							Key:    "other-custom-taint",
							Effect: v1.TaintEffectNoExecute,
							Value:  "other-custom-value",
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(machine.Spec.Taints).To(ContainElements(
				v1.Taint{
					Key:    "custom-taint",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "custom-value",
				},
				v1.Taint{
					Key:    "other-custom-taint",
					Effect: v1.TaintEffectNoExecute,
					Value:  "other-custom-value",
				},
			))

			node := test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			node = ExpectExists(ctx, env.Client, node)

			Expect(node.Spec.Taints).To(ContainElements(
				v1.Taint{
					Key:    "custom-taint",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "custom-value",
				},
				v1.Taint{
					Key:    "other-custom-taint",
					Effect: v1.TaintEffectNoExecute,
					Value:  "other-custom-value",
				},
			))
		})
		It("should sync the startupTaints to the Node when the Node comes online", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Taints: []v1.Taint{
						{
							Key:    "custom-taint",
							Effect: v1.TaintEffectNoSchedule,
							Value:  "custom-value",
						},
						{
							Key:    "other-custom-taint",
							Effect: v1.TaintEffectNoExecute,
							Value:  "other-custom-value",
						},
					},
					StartupTaints: []v1.Taint{
						{
							Key:    "custom-startup-taint",
							Effect: v1.TaintEffectNoSchedule,
							Value:  "custom-startup-value",
						},
						{
							Key:    "other-custom-startup-taint",
							Effect: v1.TaintEffectNoExecute,
							Value:  "other-custom-startup-value",
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(machine.Spec.StartupTaints).To(ContainElements(
				v1.Taint{
					Key:    "custom-startup-taint",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "custom-startup-value",
				},
				v1.Taint{
					Key:    "other-custom-startup-taint",
					Effect: v1.TaintEffectNoExecute,
					Value:  "other-custom-startup-value",
				},
			))

			node := test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			node = ExpectExists(ctx, env.Client, node)

			Expect(node.Spec.Taints).To(ContainElements(
				v1.Taint{
					Key:    "custom-taint",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "custom-value",
				},
				v1.Taint{
					Key:    "other-custom-taint",
					Effect: v1.TaintEffectNoExecute,
					Value:  "other-custom-value",
				},
				v1.Taint{
					Key:    "custom-startup-taint",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "custom-startup-value",
				},
				v1.Taint{
					Key:    "other-custom-startup-taint",
					Effect: v1.TaintEffectNoExecute,
					Value:  "other-custom-startup-value",
				},
			))
		})
		It("should not re-sync the startupTaints to the Node when the startupTaints are removed", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					StartupTaints: []v1.Taint{
						{
							Key:    "custom-startup-taint",
							Effect: v1.TaintEffectNoSchedule,
							Value:  "custom-startup-value",
						},
						{
							Key:    "other-custom-startup-taint",
							Effect: v1.TaintEffectNoExecute,
							Value:  "other-custom-startup-value",
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			node = ExpectExists(ctx, env.Client, node)

			Expect(node.Spec.Taints).To(ContainElements(
				v1.Taint{
					Key:    "custom-startup-taint",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "custom-startup-value",
				},
				v1.Taint{
					Key:    "other-custom-startup-taint",
					Effect: v1.TaintEffectNoExecute,
					Value:  "other-custom-startup-value",
				},
			))
			node.Spec.Taints = []v1.Taint{}
			ExpectApplied(ctx, env.Client, node)

			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			node = ExpectExists(ctx, env.Client, node)
			Expect(node.Spec.Taints).To(HaveLen(0))
		})
	})
	Context("Initialization", func() {
		It("should consider the Machine initialized when all initialization conditions are met", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Resources: v1alpha5.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("50Mi"),
							v1.ResourcePods:   resource.MustParse("5"),
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionFalse))

			node = ExpectExists(ctx, env.Client, node)
			node.Status.Capacity = v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			}
			node.Status.Allocatable = v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			}
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionTrue))
		})
		It("should add the initialization label to the node when the Machine is initialized", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Resources: v1alpha5.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("50Mi"),
							v1.ResourcePods:   resource.MustParse("5"),
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("10"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("80Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			node = ExpectExists(ctx, env.Client, node)
			Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.LabelNodeInitialized, "true"))
		})
		It("should not consider the Node to be initialized when the status of the Node is NotReady", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Resources: v1alpha5.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("50Mi"),
							v1.ResourcePods:   resource.MustParse("5"),
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("10"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("80Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
				ReadyStatus: v1.ConditionFalse,
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionFalse))
		})
		It("should not consider the Node to be initialized when all requested resources aren't registered", func() {
			machine := test.Machine(v1alpha5.Machine{
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
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			// Update the machine to add mock the instance type having an extended resource
			machine.Status.Capacity[fake.ResourceGPUVendorA] = resource.MustParse("2")
			machine.Status.Allocatable[fake.ResourceGPUVendorA] = resource.MustParse("2")
			ExpectApplied(ctx, env.Client, machine)

			// Extended resource hasn't registered yet by the daemonset
			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("10"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("80Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionFalse))
		})
		It("should consider the node to be initialized once all the resources are registered", func() {
			machine := test.Machine(v1alpha5.Machine{
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
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			// Update the machine to add mock the instance type having an extended resource
			machine.Status.Capacity[fake.ResourceGPUVendorA] = resource.MustParse("2")
			machine.Status.Allocatable[fake.ResourceGPUVendorA] = resource.MustParse("2")
			ExpectApplied(ctx, env.Client, machine)

			// Extended resource hasn't registered yet by the daemonset
			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("10"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("80Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionFalse))

			// Node now registers the resource
			node = ExpectExists(ctx, env.Client, node)
			node.Status.Capacity[fake.ResourceGPUVendorA] = resource.MustParse("2")
			node.Status.Allocatable[fake.ResourceGPUVendorA] = resource.MustParse("2")
			ExpectApplied(ctx, env.Client, node)

			// Reconcile the machine and the Machine/Node should now be initilized
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionTrue))
		})
		It("should not consider the Node to be initialized when all startupTaints aren't removed", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Resources: v1alpha5.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("50Mi"),
							v1.ResourcePods:   resource.MustParse("5"),
						},
					},
					StartupTaints: []v1.Taint{
						{
							Key:    "custom-startup-taint",
							Effect: v1.TaintEffectNoSchedule,
							Value:  "custom-startup-value",
						},
						{
							Key:    "other-custom-startup-taint",
							Effect: v1.TaintEffectNoExecute,
							Value:  "other-custom-startup-value",
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("10"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("80Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
			})
			ExpectApplied(ctx, env.Client, node)

			// Should add the startup taints to the node
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			node = ExpectExists(ctx, env.Client, node)
			Expect(node.Spec.Taints).To(ContainElements(
				v1.Taint{
					Key:    "custom-startup-taint",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "custom-startup-value",
				},
				v1.Taint{
					Key:    "other-custom-startup-taint",
					Effect: v1.TaintEffectNoExecute,
					Value:  "other-custom-startup-value",
				},
			))

			// Shouldn't consider the node ready since the startup taints still exist
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionFalse))
		})
		It("should consider the Node to be initialized once the startup taints are removed", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Resources: v1alpha5.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("50Mi"),
							v1.ResourcePods:   resource.MustParse("5"),
						},
					},
					StartupTaints: []v1.Taint{
						{
							Key:    "custom-startup-taint",
							Effect: v1.TaintEffectNoSchedule,
							Value:  "custom-startup-value",
						},
						{
							Key:    "other-custom-startup-taint",
							Effect: v1.TaintEffectNoExecute,
							Value:  "other-custom-startup-value",
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("10"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("80Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
			})
			ExpectApplied(ctx, env.Client, node)

			// Shouldn't consider the node ready since the startup taints still exist
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionFalse))

			node = ExpectExists(ctx, env.Client, node)
			node.Spec.Taints = []v1.Taint{}
			ExpectApplied(ctx, env.Client, node)

			// Machine should now be ready since all startup taints are removed
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
			Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineInitialized).Status).To(Equal(v1.ConditionTrue))
		})
	})
	Context("Liveness", func() {
		It("should delete the Machine when the Node hasn't registered to the Machine past the liveness TTL", func() {
			machine := test.Machine(v1alpha5.Machine{
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
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			// If the node hasn't registered in the liveness timeframe, then we deprovision the Machine
			fakeClock.Step(time.Minute * 20)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine)) // Reconcile again to handle termination flow
			ExpectNotFound(ctx, env.Client, machine)
		})
		It("should not delete the Machine when the Node hasn't registered to the Machine past the liveness TTL if ttlAfterNotRegistered is disabled", func() {
			s := test.Settings()
			s.TTLAfterNotRegistered = nil
			ctx = settings.ToContext(ctx, s)
			machine := test.Machine(v1alpha5.Machine{
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
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			// If the node hasn't registered in the liveness timeframe, then we deprovision the Machine
			fakeClock.Step(time.Minute * 20)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine)) // Reconcile again to handle termination flow
			ExpectExists(ctx, env.Client, machine)
		})
	})
	Context("Termination", func() {
		It("should cordon, drain, and delete the Machine on terminate", func() {
			machine := test.Machine(v1alpha5.Machine{
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
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("10"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("80Mi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			Expect(cloudProvider.CreatedMachines).To(HaveLen(1))

			// Kickoff the deletion flow for the machine
			Expect(env.Client.Delete(ctx, machine)).To(Succeed())

			// Machine should delete and the Node deletion should cascade shortly after
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectNotFound(ctx, env.Client, machine)
		})
	})
})
