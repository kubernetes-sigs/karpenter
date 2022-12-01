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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
)

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
	// Kubelet are options passed to the kubelet when provisioning nodes
	// +optional
	Kubelet *v1alpha5.KubeletConfiguration `json:"kubelet,omitempty"`
	// MachineTemplateRef is a reference to an object that defines provider specific configuration
	MachineTemplateRef v1.ObjectReference `json:"nodeTemplateRef,omitempty"`
	// Provider contains fields specific to your cloudprovider.
	// Deprecated: NodeTemplateRef is favored over Provider
	// +kubebuilder:pruning:PreserveUnknownFields
	Provider *v1alpha5.Provider `json:"provider,omitempty"`
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

func (in *Machine) Owner() (client.ObjectKey, bool) {
	ref, ok := lo.Find(in.OwnerReferences, func(o metav1.OwnerReference) bool {
		return o.APIVersion == v1alpha5.SchemeGroupVersion.String() &&
			o.Kind == v1alpha5.ProvisionerKind
	})
	if !ok {
		return client.ObjectKey{}, false
	}
	return client.ObjectKey{Name: ref.Name}, true
}
