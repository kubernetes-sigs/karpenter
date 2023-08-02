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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
)

// NodeClaimTemplate encapsulates the fields required to create a node and mirrors
// the fields in Provisioner. These structs are maintained separately in order
// for fields like Requirements to be able to be stored more efficiently.
type NodeClaimTemplate struct {
	v1beta1.NodeClaimTemplate

	OwnerName           string
	InstanceTypeOptions cloudprovider.InstanceTypes
	Requirements        scheduling.Requirements

	FromProvisioner bool // Whether this NodeClaimTemplate comes from a real NodePool or a converted NodePool
}

func NewNodeClaimTemplate(nodePool *v1beta1.NodePool) *NodeClaimTemplate {
	nct := &NodeClaimTemplate{
		NodeClaimTemplate: nodePool.Spec.Template,
		OwnerName:         nodePool.Name,
		Requirements:      scheduling.NewRequirements(),
		FromProvisioner:   nodePool.IsProvisioner,
	}
	if nodePool.IsProvisioner {
		nct.Labels = lo.Assign(nct.Labels, map[string]string{v1alpha5.ProvisionerNameLabelKey: nodePool.Name})
	} else {
		nct.Labels = lo.Assign(nct.Labels, map[string]string{v1beta1.NodePoolLabelKey: nodePool.Name})
	}
	nct.Requirements.Add(scheduling.NewNodeSelectorRequirements(nodePool.Spec.Template.Spec.Requirements...).Values()...)
	nct.Requirements.Add(scheduling.NewLabelRequirements(nodePool.Spec.Template.Labels).Values()...)
	return nct
}

func (i *NodeClaimTemplate) OwnerKey() nodepoolutil.Key {
	return nodepoolutil.Key{Name: i.OwnerName, IsProvisioner: i.FromProvisioner}
}

func (i *NodeClaimTemplate) OwnerKind() string {
	return lo.Ternary(i.FromProvisioner, "provisioner", "nodepool")
}

func (i *NodeClaimTemplate) ToNodeClaim(nodePool *v1beta1.NodePool) *v1beta1.NodeClaim {
	// Order the instance types by price and only take the first 100 of them to decrease the instance type size in the requirements
	instanceTypes := lo.Slice(i.InstanceTypeOptions.OrderByPrice(i.Requirements), 0, 100)
	i.Requirements.Add(scheduling.NewRequirement(v1.LabelInstanceTypeStable, v1.NodeSelectorOpIn, lo.Map(instanceTypes, func(i *cloudprovider.InstanceType, _ int) string {
		return i.Name
	})...))

	m := &v1beta1.NodeClaim{
		ObjectMeta: i.ObjectMeta,
		Spec:       i.Spec,
	}
	// TODO @joinnis: Figure out how to calculate the NodePool hash
	// m.Annotations = lo.Assign(m.Annotations, map[string]string{v1alpha5.ProvisionerHashAnnotationKey: provisionerDriftHash})
	m.ObjectMeta.GenerateName = fmt.Sprintf("%s-", i.OwnerName)
	m.Spec.Requirements = i.Requirements.NodeSelectorRequirements()
	lo.Must0(controllerutil.SetOwnerReference(nodePool, m, scheme.Scheme))
	return m
}

func (i *NodeClaimTemplate) ToMachine(provisioner *v1alpha5.Provisioner) *v1alpha5.Machine {
	// Order the instance types by price and only take the first 100 of them to decrease the instance type size in the requirements
	instanceTypes := lo.Slice(i.InstanceTypeOptions.OrderByPrice(i.Requirements), 0, 100)
	i.Requirements.Add(scheduling.NewRequirement(v1.LabelInstanceTypeStable, v1.NodeSelectorOpIn, lo.Map(instanceTypes, func(i *cloudprovider.InstanceType, _ int) string {
		return i.Name
	})...))

	m := &v1alpha5.Machine{
		ObjectMeta: i.ObjectMeta,
		Spec: v1alpha5.MachineSpec{
			Taints:        i.NodeClaimTemplate.Spec.Taints,
			StartupTaints: i.NodeClaimTemplate.Spec.StartupTaints,
			Requirements:  i.Requirements.NodeSelectorRequirements(),
			Resources: v1alpha5.ResourceRequirements{
				Requests: i.NodeClaimTemplate.Spec.Resources.Requests,
			},
		},
	}
	if i.NodeClaimTemplate.Spec.KubeletConfiguration != nil {
		m.Spec.Kubelet = &v1alpha5.KubeletConfiguration{
			ClusterDNS:                  i.NodeClaimTemplate.Spec.KubeletConfiguration.ClusterDNS,
			ContainerRuntime:            i.NodeClaimTemplate.Spec.KubeletConfiguration.ContainerRuntime,
			MaxPods:                     i.NodeClaimTemplate.Spec.KubeletConfiguration.MaxPods,
			PodsPerCore:                 i.NodeClaimTemplate.Spec.KubeletConfiguration.PodsPerCore,
			SystemReserved:              i.NodeClaimTemplate.Spec.KubeletConfiguration.SystemReserved,
			KubeReserved:                i.NodeClaimTemplate.Spec.KubeletConfiguration.KubeReserved,
			EvictionHard:                i.NodeClaimTemplate.Spec.KubeletConfiguration.EvictionHard,
			EvictionSoft:                i.NodeClaimTemplate.Spec.KubeletConfiguration.EvictionSoft,
			EvictionSoftGracePeriod:     i.NodeClaimTemplate.Spec.KubeletConfiguration.EvictionSoftGracePeriod,
			EvictionMaxPodGracePeriod:   i.NodeClaimTemplate.Spec.KubeletConfiguration.EvictionMaxPodGracePeriod,
			ImageGCHighThresholdPercent: i.NodeClaimTemplate.Spec.KubeletConfiguration.ImageGCHighThresholdPercent,
			ImageGCLowThresholdPercent:  i.NodeClaimTemplate.Spec.KubeletConfiguration.ImageGCLowThresholdPercent,
			CPUCFSQuota:                 i.NodeClaimTemplate.Spec.KubeletConfiguration.CPUCFSQuota,
		}
	}
	if i.NodeClaimTemplate.Spec.NodeClass != nil {
		m.Spec.MachineTemplateRef = &v1alpha5.MachineTemplateRef{
			Kind:       i.NodeClaimTemplate.Spec.NodeClass.Kind,
			Name:       i.NodeClaimTemplate.Spec.NodeClass.Name,
			APIVersion: i.NodeClaimTemplate.Spec.NodeClass.APIVersion,
		}
	}
	// TODO @joinnis: Figure out how to calculate the Provisioner hash
	// m.Annotations = lo.Assign(m.Annotations, map[string]string{v1alpha5.ProvisionerHashAnnotationKey: provisionerDriftHash})
	m.ObjectMeta.GenerateName = fmt.Sprintf("%s-", i.OwnerName)
	lo.Must0(controllerutil.SetOwnerReference(provisioner, m, scheme.Scheme))
	return m
}
