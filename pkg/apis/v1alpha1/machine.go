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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

// MachineSpec describes the desired state of the Machine
type MachineSpec struct {
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
	// Resources models the resource requirements for the Machine to launch
	Resources ResourceRequirements `json:"resources,omitempty"`
	// Kubelet are options passed to the kubelet when provisioning nodes
	// +optional
	Kubelet *v1alpha5.KubeletConfiguration `json:"kubelet,omitempty"`
	// MachineTemplateRef is a reference to an object that defines provider specific configuration
	MachineTemplateRef *v1.ObjectReference `json:"machineTemplateRef,omitempty"`
}

// ResourceRequirements models the required resources for the Machine to launch
// Ths will eventually be transformed into v1.ResourceRequirements when we support resources.limits
type ResourceRequirements struct {
	// Requests describes the minimum required resources for the Machine to launch
	// +optional
	Requests v1.ResourceList `json:"requests,omitempty"`
}

// Machine is the Schema for the Machines API
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=machines,scope=Cluster,categories=karpenter
// +kubebuilder:subresource:status
type Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineSpec   `json:"spec,omitempty"`
	Status MachineStatus `json:"status,omitempty"`
}

// MachineList contains a list of Provisioner
// +kubebuilder:object:root=true
type MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Machine `json:"items"`
}

// MachineFromNode converts a node into a pseudo-Machine using known values from the node
func MachineFromNode(node *v1.Node) *Machine {
	m := &Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Annotations: node.Annotations,
			Labels:      node.Labels,
		},
		Spec: MachineSpec{
			Taints:       node.Spec.Taints,
			Requirements: scheduling.NewLabelRequirements(node.Labels).NodeSelectorRequirements(),
			Resources: ResourceRequirements{
				Requests: node.Status.Allocatable,
			},
		},
		Status: MachineStatus{
			ProviderID:  node.Spec.ProviderID,
			Allocatable: node.Status.Allocatable,
		},
	}
	if _, ok := node.Labels[v1alpha5.LabelNodeInitialized]; ok {
		m.StatusConditions().MarkTrue(MachineInitialized)
	}
	m.StatusConditions().MarkTrue(MachineCreated)
	m.StatusConditions().MarkTrue(MachineRegistered)
	//controllerutil.AddFinalizer(m, v1alpha5.TerminationFinalizer)
	return m
}
