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

package scheduling

import (
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha1"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
)

// MachineTemplate encapsulates the fields required to create a node and mirrors
// the fields in Provisioner. These structs are maintained separately in order
// for fields like Requirements to be able to be stored more efficiently.
type MachineTemplate struct {
	ProvisionerRef       *v1alpha5.Provisioner
	Provider             *v1alpha5.Provider
	ProviderRef          *v1alpha5.ProviderRef
	Annotations          map[string]string
	Labels               map[string]string
	Taints               Taints
	StartupTaints        Taints
	Requirements         Requirements
	KubeletConfiguration *v1alpha5.KubeletConfiguration
}

func NewMachineTemplate(provisioner *v1alpha5.Provisioner) *MachineTemplate {
	labels := lo.Assign(provisioner.Spec.Labels, map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name})
	requirements := NewRequirements()
	requirements.Add(NewNodeSelectorRequirements(provisioner.Spec.Requirements...).Values()...)
	requirements.Add(NewLabelRequirements(labels).Values()...)
	return &MachineTemplate{
		ProvisionerRef:       provisioner,
		Provider:             provisioner.Spec.Provider,
		ProviderRef:          provisioner.Spec.ProviderRef,
		KubeletConfiguration: provisioner.Spec.KubeletConfiguration,
		Annotations:          provisioner.Spec.Annotations,
		Labels:               labels,
		Taints:               provisioner.Spec.Taints,
		StartupTaints:        provisioner.Spec.StartupTaints,
		Requirements:         requirements,
	}
}

func (n *MachineTemplate) ToMachine() *v1alpha1.Machine {
	return &v1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: n.ProvisionerRef.Name,
			OwnerReferences: []metav1.OwnerReference{
				{},
			},
		},
		Spec: v1alpha1.MachineSpec{
			Labels:          n.Labels,
			Taints:          n.Taints,
			StartupTaints:   n.StartupTaints,
			Requirements:    n.Requirements.NodeSelectorRequirements(),
			Kubelet:         n.KubeletConfiguration,
			NodeTemplateRef: n.ProviderRef,
		},
	}
}
