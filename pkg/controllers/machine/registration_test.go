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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Registration", func() {
	var provisioner *v1alpha5.Provisioner

	BeforeEach(func() {
		provisioner = test.Provisioner()
	})
	It("should match the Machine to the Node when the Node comes online", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		machine = ExpectExists(ctx, env.Client, machine)

		node := test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(ExpectStatusConditionExists(machine, v1alpha5.MachineRegistered).Status).To(Equal(v1.ConditionTrue))
	})
	It("should add the owner reference to the Node when the Node comes online", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
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
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					"custom-label":                   "custom-value",
					"other-custom-label":             "other-custom-value",
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
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
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
				Annotations: map[string]string{
					v1alpha5.DoNotConsolidateNodeAnnotationKey: "true",
					"my-custom-annotation":                     "my-custom-value",
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
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
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
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
		ExpectApplied(ctx, env.Client, provisioner, machine)
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
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
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
		ExpectApplied(ctx, env.Client, provisioner, machine)
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
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
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
		ExpectApplied(ctx, env.Client, provisioner, machine)
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
