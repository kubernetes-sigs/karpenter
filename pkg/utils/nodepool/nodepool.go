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

package nodepool

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	provisionerutil "github.com/aws/karpenter-core/pkg/utils/provisioner"
)

type Key struct {
	Name          string
	IsProvisioner bool
}

func New(provisioner *v1alpha5.Provisioner) *v1beta1.NodePool {
	np := &v1beta1.NodePool{
		ObjectMeta: provisioner.ObjectMeta,
		Spec: v1beta1.NodePoolSpec{
			Template: v1beta1.NodeClaimTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: provisioner.Spec.Annotations,
					Labels:      provisioner.Spec.Labels,
				},
				Spec: v1beta1.NodeClaimSpec{
					Taints:               provisioner.Spec.Taints,
					StartupTaints:        provisioner.Spec.StartupTaints,
					Requirements:         provisioner.Spec.Requirements,
					KubeletConfiguration: NewKubeletConfiguration(provisioner.Spec.KubeletConfiguration),
					NodeClass:            NewNodeClassReference(provisioner.Spec.ProviderRef),
					Provider:             provisioner.Spec.Provider,
				},
			},
			Weight: provisioner.Spec.Weight,
		},
		IsProvisioner: true,
	}
	if provisioner.Spec.TTLSecondsUntilExpired != nil {
		np.Spec.Deprovisioning.ExpirationTTL = metav1.Duration{Duration: lo.Must(time.ParseDuration(fmt.Sprintf("%ds", lo.FromPtr[int64](provisioner.Spec.TTLSecondsUntilExpired))))}
	} else {
		np.Spec.Deprovisioning.ExpirationTTL = metav1.Duration{Duration: -1}
	}
	if provisioner.Spec.Consolidation != nil && lo.FromPtr(provisioner.Spec.Consolidation.Enabled) {
		np.Spec.Deprovisioning.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
	} else if provisioner.Spec.TTLSecondsAfterEmpty != nil {
		np.Spec.Deprovisioning.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenEmpty
		np.Spec.Deprovisioning.ConsolidationTTL = metav1.Duration{Duration: lo.Must(time.ParseDuration(fmt.Sprintf("%ds", lo.FromPtr[int64](provisioner.Spec.TTLSecondsAfterEmpty))))}
	} else {
		np.Spec.Deprovisioning.ConsolidationPolicy = v1beta1.ConsolidationPolicyNever
	}
	if provisioner.Spec.Limits != nil {
		np.Spec.Limits = v1beta1.Limits(provisioner.Spec.Limits.Resources)
	}
	return np
}

func NewKubeletConfiguration(kc *v1alpha5.KubeletConfiguration) *v1beta1.KubeletConfiguration {
	if kc == nil {
		return nil
	}
	return &v1beta1.KubeletConfiguration{
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

func NewNodeClassReference(pr *v1alpha5.MachineTemplateRef) *v1beta1.NodeClassReference {
	if pr == nil {
		return nil
	}
	return &v1beta1.NodeClassReference{
		Kind:       pr.Kind,
		Name:       pr.Name,
		APIVersion: pr.APIVersion,
	}
}

func List(ctx context.Context, c client.Client, opts ...client.ListOption) (*v1beta1.NodePoolList, error) {
	provisionerList := &v1alpha5.ProvisionerList{}
	if err := c.List(ctx, provisionerList, opts...); err != nil {
		return nil, err
	}
	convertedNodePools := lo.Map(provisionerList.Items, func(p v1alpha5.Provisioner, _ int) v1beta1.NodePool {
		return *New(&p)
	})
	// TODO @joinnis: Add NodePools to this List() function when releasing v1beta1 APIs
	nodePoolList := &v1beta1.NodePoolList{}
	nodePoolList.Items = append(nodePoolList.Items, convertedNodePools...)
	return nodePoolList, nil
}

func Patch(ctx context.Context, c client.Client, stored, nodePool *v1beta1.NodePool) error {
	if nodePool.IsProvisioner {
		storedProvisioner := provisionerutil.New(stored)
		provisioner := provisionerutil.New(nodePool)
		return c.Patch(ctx, provisioner, client.MergeFrom(storedProvisioner))
	}
	return c.Patch(ctx, nodePool, client.MergeFrom(stored))
}

func PatchStatus(ctx context.Context, c client.Client, stored, nodePool *v1beta1.NodePool) error {
	if nodePool.IsProvisioner {
		storedProvisioner := provisionerutil.New(stored)
		provisioner := provisionerutil.New(nodePool)
		return c.Status().Patch(ctx, provisioner, client.MergeFrom(storedProvisioner))
	}
	return c.Status().Patch(ctx, nodePool, client.MergeFrom(stored))
}

func HashAnnotation(nodePool *v1beta1.NodePool) map[string]string {
	if nodePool.IsProvisioner {
		provisioner := provisionerutil.New(nodePool)
		return map[string]string{v1alpha5.ProvisionerHashAnnotationKey: provisioner.Hash()}
	}
	return map[string]string{v1beta1.NodePoolHashAnnotationKey: nodePool.Hash()}
}
