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

package provisioner

import (
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
)

func New(nodePool *v1beta1.NodePool) *v1alpha5.Provisioner {
	p := &v1alpha5.Provisioner{
		TypeMeta:   nodePool.TypeMeta,
		ObjectMeta: nodePool.ObjectMeta,
		Spec: v1alpha5.ProvisionerSpec{
			Annotations:          nodePool.Spec.Template.Annotations,
			Labels:               nodePool.Spec.Template.Labels,
			Taints:               nodePool.Spec.Template.Spec.Taints,
			StartupTaints:        nodePool.Spec.Template.Spec.StartupTaints,
			Requirements:         nodePool.Spec.Template.Spec.Requirements,
			KubeletConfiguration: NewKubeletConfiguration(nodePool.Spec.Template.Spec.KubeletConfiguration),
			Provider:             nodePool.Spec.Template.Spec.Provider,
			ProviderRef:          NewProviderRef(nodePool.Spec.Template.Spec.NodeClass),
			Limits:               NewLimits(v1.ResourceList(nodePool.Spec.Limits)),
			Weight:               nodePool.Spec.Weight,
		},
		Status: v1alpha5.ProvisionerStatus{
			Resources: nodePool.Status.Resources,
		},
	}
	if nodePool.Spec.Disruption.ExpireAfter.Duration != nil {
		p.Spec.TTLSecondsUntilExpired = lo.ToPtr(int64(nodePool.Spec.Disruption.ExpireAfter.Seconds()))
	}
	if nodePool.Spec.Disruption.ConsolidationPolicy == v1beta1.ConsolidationPolicyWhenEmpty {
		p.Spec.TTLSecondsAfterEmpty = lo.ToPtr(int64(nodePool.Spec.Disruption.ConsolidateAfter.Seconds()))
	}
	if nodePool.Spec.Disruption.ConsolidationPolicy == v1beta1.ConsolidationPolicyWhenUnderutilized {
		p.Spec.Consolidation = &v1alpha5.Consolidation{
			Enabled: lo.ToPtr(true),
		}
	}
	return p
}

func NewKubeletConfiguration(kc *v1beta1.KubeletConfiguration) *v1alpha5.KubeletConfiguration {
	if kc == nil {
		return nil
	}
	return &v1alpha5.KubeletConfiguration{
		ClusterDNS:                  kc.ClusterDNS,
		ContainerRuntime:            kc.ContainerRuntime,
		MaxPods:                     kc.MaxPods,
		PodsPerCore:                 kc.PodsPerCore,
		SystemReserved:              kc.SystemReserved,
		KubeReserved:                kc.KubeReserved,
		EvictionHard:                kc.EvictionHard,
		EvictionSoft:                kc.EvictionSoft,
		EvictionSoftGracePeriod:     kc.EvictionSoftGracePeriod,
		EvictionMaxPodGracePeriod:   kc.EvictionMaxPodGracePeriod,
		ImageGCHighThresholdPercent: kc.ImageGCHighThresholdPercent,
		ImageGCLowThresholdPercent:  kc.ImageGCLowThresholdPercent,
		CPUCFSQuota:                 kc.CPUCFSQuota,
	}
}

func NewProviderRef(nc *v1beta1.NodeClassReference) *v1alpha5.MachineTemplateRef {
	if nc == nil {
		return nil
	}
	return &v1alpha5.MachineTemplateRef{
		Kind:       nc.Kind,
		Name:       nc.Name,
		APIVersion: nc.APIVersion,
	}
}

func NewLimits(limits v1.ResourceList) *v1alpha5.Limits {
	return &v1alpha5.Limits{
		Resources: limits,
	}
}
