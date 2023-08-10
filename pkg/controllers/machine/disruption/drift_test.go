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
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"

	"github.com/aws/karpenter-core/pkg/operator/controller"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/machine/disruption"
	controllerprov "github.com/aws/karpenter-core/pkg/controllers/provisioner"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Drift", func() {
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
				},
				Annotations: map[string]string{
					v1alpha5.ProvisionerHashAnnotationKey: provisioner.Hash(),
				},
			},
		})
		// Machines are required to be launched before they can be evaluated for drift
		machine.StatusConditions().MarkTrue(v1alpha5.MachineLaunched)
	})
	It("should detect drift", func() {
		cp.Drifted = "drifted"
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()).To(BeTrue())
	})
	It("should detect static drift before cloud provider drift", func() {
		cp.Drifted = "drifted"
		provisioner.Annotations = lo.Assign(provisioner.Annotations, map[string]string{
			v1alpha5.ProvisionerHashAnnotationKey: "123456789",
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).Reason).To(Equal(string(disruption.ProvisionerStaticallyDrifted)))
	})
	It("should detect node requirement drift before cloud provider drift", func() {
		cp.Drifted = "drifted"
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			v1.NodeSelectorRequirement{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpDoesNotExist,
			},
		}
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).Reason).To(Equal(string(disruption.NodeRequirementDrifted)))
	})
	It("should not detect drift if the feature flag is disabled", func() {
		cp.Drifted = "drifted"
		ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should remove the status condition from the machine if the feature flag is disabled", func() {
		cp.Drifted = "drifted"
		ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		ExpectApplied(ctx, env.Client, provisioner, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should remove the status condition from the machine when the machine launch condition is false", func() {
		cp.Drifted = "drifted"
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		machine.StatusConditions().MarkFalse(v1alpha5.MachineLaunched, "", "")
		ExpectApplied(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should remove the status condition from the machine when the machine launch condition doesn't exist", func() {
		cp.Drifted = "drifted"
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		machine.Status.Conditions = lo.Reject(machine.Status.Conditions, func(s apis.Condition, _ int) bool {
			return s.Type == v1alpha5.MachineLaunched
		})
		ExpectApplied(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should not detect drift if the provisioner does not exist", func() {
		cp.Drifted = "drifted"
		ExpectApplied(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should remove the status condition from the machine if the machine is no longer drifted", func() {
		cp.Drifted = ""
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		ExpectApplied(ctx, env.Client, provisioner, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	Context("NodeRequirement Drift", func() {
		DescribeTable("",
			func(oldProvisionerReq []v1.NodeSelectorRequirement, newProvisionerReq []v1.NodeSelectorRequirement, machineLabels map[string]string, drifted bool) {
				cp.Drifted = ""
				provisioner.Spec.Requirements = oldProvisionerReq
				machine.Labels = lo.Assign(machine.Labels, machineLabels)

				ExpectApplied(ctx, env.Client, provisioner, machine)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
				machine = ExpectExists(ctx, env.Client, machine)
				Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())

				provisioner.Spec.Requirements = newProvisionerReq
				ExpectApplied(ctx, env.Client, provisioner)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
				machine = ExpectExists(ctx, env.Client, machine)
				if drifted {
					Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()).To(BeTrue())
				} else {
					Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
				}
			},
			Entry(
				"should return drifted if the provisioner node requirement is updated",
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.ArchitectureAmd64}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeSpot}},
				},
				map[string]string{
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
					v1.LabelArchStable:         v1alpha5.ArchitectureAmd64,
					v1.LabelOSStable:           string(v1.Linux),
				},
				true),
			Entry(
				"should return drifted if a new node requirement is added",
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
					{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.ArchitectureAmd64}},
				},
				map[string]string{
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
					v1.LabelOSStable:           string(v1.Linux),
				},
				true,
			),
			Entry(
				"should return drifted if a node requirement is reduced",
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Windows)}},
				},
				map[string]string{
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
					v1.LabelOSStable:           string(v1.Linux),
				},
				true,
			),
			Entry(
				"should not return drifted if a node requirement is expanded",
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}},
				},
				map[string]string{
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
					v1.LabelOSStable:           string(v1.Linux),
				},
				false,
			),
			Entry(
				"should not return drifted if a node requirement set to Exists",
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpExists, Values: []string{}},
				},
				map[string]string{
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
					v1.LabelOSStable:           string(v1.Linux),
				},
				false,
			),
			Entry(
				"should return drifted if a node requirement set to DoesNotExists",
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpDoesNotExist, Values: []string{}},
				},
				map[string]string{
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
					v1.LabelOSStable:           string(v1.Linux),
				},
				true,
			),
			Entry(
				"should not return drifted if a machine is grater then node requirement",
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpGt, Values: []string{"2"}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpGt, Values: []string{"10"}},
				},
				map[string]string{
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
					v1.LabelInstanceTypeStable: "5",
				},
				true,
			),
			Entry(
				"should not return drifted if a machine is less then node requirement",
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpLt, Values: []string{"5"}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
					{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}},
				},
				map[string]string{
					v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
					v1.LabelInstanceTypeStable: "2",
				},
				true,
			),
		)
		It("should return drifted only on machines that are drifted from an updated provisioner", func() {
			cp.Drifted = ""
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
				{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}},
			}
			machine.Labels = lo.Assign(machine.Labels, map[string]string{
				v1alpha5.LabelCapacityType: v1alpha5.CapacityTypeOnDemand,
				v1.LabelOSStable:           string(v1.Linux),
			})
			machineTwo, _ := test.MachineAndNode(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       test.RandomName(),
						v1alpha5.LabelCapacityType:       v1alpha5.CapacityTypeOnDemand,
						v1.LabelOSStable:                 string(v1.Windows),
					},
					Annotations: map[string]string{
						v1alpha5.ProvisionerHashAnnotationKey: provisioner.Hash(),
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			machineTwo.StatusConditions().MarkTrue(v1alpha5.MachineLaunched)
			ExpectApplied(ctx, env.Client, provisioner, machine, machineTwo)

			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machineTwo))
			machine = ExpectExists(ctx, env.Client, machine)
			machineTwo = ExpectExists(ctx, env.Client, machineTwo)
			Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
			Expect(machineTwo.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())

			// Removed Windows OS
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{v1alpha5.CapacityTypeOnDemand}},
				{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
			}
			ExpectApplied(ctx, env.Client, provisioner)

			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())

			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machineTwo))
			machineTwo = ExpectExists(ctx, env.Client, machineTwo)
			Expect(machineTwo.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()).To(BeTrue())
		})

	})
	Context("Provisioner Static Drift", func() {
		var testProvisionerOptions test.ProvisionerOptions
		var provisionerController controller.Controller
		BeforeEach(func() {
			cp.Drifted = ""
			provisionerController = controllerprov.NewController(env.Client)
			testProvisionerOptions = test.ProvisionerOptions{
				ObjectMeta: provisioner.ObjectMeta,
				Taints: []v1.Taint{
					{
						Key:    "keyValue1",
						Effect: v1.TaintEffectNoExecute,
					},
				},
				StartupTaints: []v1.Taint{
					{
						Key:    "startupKeyValue1",
						Effect: v1.TaintEffectNoExecute,
					},
				},
				Labels: map[string]string{
					"keyLabel":  "valueLabel",
					"keyLabel2": "valueLabel2",
				},
				Kubelet: &v1alpha5.KubeletConfiguration{
					MaxPods: ptr.Int32(10),
				},
				Annotations: map[string]string{
					"keyAnnotation":  "valueAnnotation",
					"keyAnnotation2": "valueAnnotation2",
				},
			}
			provisioner = test.Provisioner(testProvisionerOptions)
			machine.ObjectMeta.Annotations[v1alpha5.ProvisionerHashAnnotationKey] = provisioner.Hash()
		})
		It("should detect drift on changes for all static fields", func() {
			ExpectApplied(ctx, env.Client, provisioner, machine)
			ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())

			// Change one static field for the same provisioner
			provisionerFieldToChange := []*v1alpha5.Provisioner{
				test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{ObjectMeta: provisioner.ObjectMeta, Annotations: map[string]string{"keyAnnotationTest": "valueAnnotationTest"}}),
				test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{ObjectMeta: provisioner.ObjectMeta, Labels: map[string]string{"keyLabelTest": "valueLabelTest"}}),
				test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{ObjectMeta: provisioner.ObjectMeta, Taints: []v1.Taint{{Key: "keytest2Taint", Effect: v1.TaintEffectNoExecute}}}),
				test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{ObjectMeta: provisioner.ObjectMeta, StartupTaints: []v1.Taint{{Key: "keytest2StartupTaint", Effect: v1.TaintEffectNoExecute}}}),
				test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{ObjectMeta: provisioner.ObjectMeta, Kubelet: &v1alpha5.KubeletConfiguration{MaxPods: ptr.Int32(30)}}),
			}

			for _, updatedProvisioner := range provisionerFieldToChange {
				ExpectApplied(ctx, env.Client, updatedProvisioner)
				ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(updatedProvisioner))
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
				machine = ExpectExists(ctx, env.Client, machine)
				Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()).To(BeTrue())
			}
		})
		It("should not return drifted if karpenter.sh/provisioner-hash annotation is not present on the provisioner", func() {
			provisioner.ObjectMeta.Annotations = map[string]string{}
			ExpectApplied(ctx, env.Client, provisioner, machine)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
		})
		It("should not return drifted if karpenter.sh/provisioner-hash annotation is not present on the machine", func() {
			machine.ObjectMeta.Annotations = map[string]string{}
			ExpectApplied(ctx, env.Client, provisioner, machine)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
			machine = ExpectExists(ctx, env.Client, machine)
			Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
		})
	})
})
