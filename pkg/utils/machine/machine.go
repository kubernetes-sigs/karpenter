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

package machine

import (
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

// New converts a node into a Machine using known values from the node and provisioner spec values
// Deprecated: This Machine generator function can be removed when v1alpha6 migration has completed.
func New(node *v1.Node, provisioner *v1alpha5.Provisioner) *v1alpha5.Machine {
	machine := NewFromNode(node)
	machine.Annotations = lo.Assign(machine.Annotations, v1alpha5.ProviderAnnotation(provisioner.Spec.Provider))
	machine.Spec.Kubelet = provisioner.Spec.KubeletConfiguration
	machine.Spec.Taints = provisioner.Spec.Taints
	machine.Spec.Requirements = provisioner.Spec.Requirements
	machine.Spec.StartupTaints = provisioner.Spec.StartupTaints
	machine.Spec.MachineTemplateRef = provisioner.Spec.ProviderRef
	lo.Must0(controllerutil.SetOwnerReference(provisioner, machine, scheme.Scheme))

	return machine
}

// NewFromNode converts a node into a pseudo-Machine using known values from the node
// Deprecated: This Machine generator function can be removed when v1alpha6 migration has completed.
func NewFromNode(node *v1.Node) *v1alpha5.Machine {
	m := &v1alpha5.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Annotations: node.Annotations,
			Labels:      node.Labels,
			Finalizers:  []string{v1alpha5.TerminationFinalizer},
		},
		Spec: v1alpha5.MachineSpec{
			Taints:       node.Spec.Taints,
			Requirements: scheduling.NewLabelRequirements(node.Labels).NodeSelectorRequirements(),
			Resources: v1alpha5.ResourceRequirements{
				Requests: node.Status.Allocatable,
			},
		},
		Status: v1alpha5.MachineStatus{
			ProviderID:  node.Spec.ProviderID,
			Capacity:    node.Status.Capacity,
			Allocatable: node.Status.Allocatable,
		},
	}
	if _, ok := node.Labels[v1alpha5.LabelNodeInitialized]; ok {
		m.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
	}
	m.StatusConditions().MarkTrue(v1alpha5.MachineCreated)
	m.StatusConditions().MarkTrue(v1alpha5.MachineRegistered)
	return m
}
