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
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/ptr"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

// MachineTemplate encapsulates the fields required to create a node and mirrors
// the fields in Provisioner. These structs are maintained separately in order
// for fields like Requirements to be able to be stored more efficiently.
type MachineTemplate struct {
	ProvisionerName     string
	InstanceTypeOptions cloudprovider.InstanceTypes
	Provider            *v1alpha5.Provider
	ProviderRef         *v1alpha5.MachineTemplateRef
	Annotations         map[string]string
	Labels              map[string]string
	Taints              scheduling.Taints
	StartupTaints       scheduling.Taints
	Requirements        scheduling.Requirements
	Requests            v1.ResourceList
	Kubelet             *v1alpha5.KubeletConfiguration
}

func NewMachineTemplate(provisioner *v1alpha5.Provisioner) *MachineTemplate {
	labels := lo.Assign(provisioner.Spec.Labels, map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name})
	requirements := scheduling.NewRequirements()
	requirements.Add(scheduling.NewNodeSelectorRequirements(provisioner.Spec.Requirements...).Values()...)
	requirements.Add(scheduling.NewLabelRequirements(labels).Values()...)
	return &MachineTemplate{
		ProvisionerName: provisioner.Name,
		Provider:        provisioner.Spec.Provider,
		ProviderRef:     provisioner.Spec.ProviderRef,
		Kubelet:         provisioner.Spec.KubeletConfiguration,
		Annotations:     provisioner.Spec.Annotations,
		Labels:          labels,
		Taints:          provisioner.Spec.Taints,
		StartupTaints:   provisioner.Spec.StartupTaints,
		Requirements:    requirements,
	}
}

func (i *MachineTemplate) ToNode() *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      lo.Assign(i.Labels, i.Requirements.Labels()),
			Annotations: i.Annotations,
			Finalizers:  []string{v1alpha5.TerminationFinalizer},
		},
		Spec: v1.NodeSpec{
			Taints: append(i.Taints, i.StartupTaints...),
		},
	}
}

func (i *MachineTemplate) ToMachine(owner *v1alpha5.Provisioner) *v1alpha5.Machine {
	// Order the instance types by price and only take the first 100 of them to decrease the instance type size in the requirements
	instanceTypes := lo.Slice(i.InstanceTypeOptions.OrderByPrice(i.Requirements), 0, 100)
	i.Requirements.Add(scheduling.NewRequirement(v1.LabelInstanceTypeStable, v1.NodeSelectorOpIn, lo.Map(instanceTypes, func(i *cloudprovider.InstanceType, _ int) string {
		return i.Name
	})...))
	m := &v1alpha5.Machine{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", i.ProvisionerName),
			Annotations:  lo.Assign(i.Annotations, v1alpha5.ProviderAnnotation(i.Provider)),
			Labels:       i.Labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         v1alpha5.SchemeGroupVersion.String(),
					Kind:               "Provisioner",
					Name:               owner.Name,
					UID:                owner.UID,
					BlockOwnerDeletion: ptr.Bool(true),
				},
			},
		},
		Spec: v1alpha5.MachineSpec{
			Taints:        i.Taints,
			StartupTaints: i.StartupTaints,
			Requirements:  i.Requirements.NodeSelectorRequirements(),
			Kubelet:       i.Kubelet,
			Resources: v1alpha5.ResourceRequirements{
				Requests: i.Requests,
			},
			MachineTemplateRef: i.ProviderRef,
		},
	}
	return m
}
