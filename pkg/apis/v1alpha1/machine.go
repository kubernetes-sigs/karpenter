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

package v1alpha1

import (
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
)

type MachineSpec struct {
	// Annotations are added and applied to the node.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// Labels are layered with Requirements and applied to the node.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Taints will be applied to the machine's node.
	// +optional
	Taints []v1.Taint `json:"taints,omitempty"`
	// StartupTaints are taints that are applied to nodes upon startup which are expected to be removed automatically
	// within a short period of time, typically by a DaemonSet that tolerates the taint. These are commonly used by
	// daemonsets to allow initialization and enforce startup ordering.  StartupTaints are ignored for provisioning
	// purposes in that pods are not required to tolerate a StartupTaint in order to have nodes provisioned for them.
	// +optional
	StartupTaints []v1.Taint `json:"startupTaints,omitempty"`
	// Requirements are layered with Labels and applied to every node.
	Requirements []v1.NodeSelectorRequirement `json:"requirements,omitempty"`
	// Kubelet are options passed to the kubelet when provisioning nodes
	// +optional
	Kubelet *v1alpha5.KubeletConfiguration `json:"kubelet,omitempty"`
	// NodeTemplateRef is a reference to an object that defines provider specific configuration
	NodeTemplateRef *v1alpha5.ProviderRef `json:"nodeTemplateRef,omitempty"`
}

// Machine is the Schema for the Machines API
type Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineSpec   `json:"spec,omitempty"`
	Status MachineStatus `json:"status,omitempty"`
}

// MachineList contains a list of Provisioner
type MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Machine `json:"items"`
}

func NewMachine(provisioner *v1alpha5.Provisioner) *Machine {
	labels := lo.Assign(provisioner.Spec.Labels, map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name})
	requirements := provisioner.Spec.Requirements
	for k, v := range labels {
		requirements = append(requirements, v1.NodeSelectorRequirement{
			Key:      k,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{v},
		})
	}
	m := &Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "placeholder", // TODO joinnis: Replace this name when we get to generating the name
		},
		Spec: MachineSpec{
			Annotations:     provisioner.Annotations,
			Labels:          labels,
			Kubelet:         provisioner.Spec.KubeletConfiguration,
			Taints:          provisioner.Spec.Taints,
			StartupTaints:   provisioner.Spec.StartupTaints,
			Requirements:    requirements,
			NodeTemplateRef: provisioner.Spec.ProviderRef,
		},
	}
	lo.Must0(controllerutil.SetOwnerReference(provisioner, m, scheme.Scheme))
	return m
}
