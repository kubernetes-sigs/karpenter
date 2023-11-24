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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
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
				ObjectMeta: v1beta1.ObjectMeta{
					Annotations: provisioner.Spec.Annotations,
					Labels:      provisioner.Spec.Labels,
				},
				Spec: v1beta1.NodeClaimSpec{
					Taints:        provisioner.Spec.Taints,
					StartupTaints: provisioner.Spec.StartupTaints,
					Requirements:  provisioner.Spec.Requirements,
					Kubelet:       NewKubeletConfiguration(provisioner.Spec.KubeletConfiguration),
					NodeClassRef:  NewNodeClassReference(provisioner.Spec.ProviderRef),
					Provider:      provisioner.Spec.Provider,
				},
			},
			Weight: provisioner.Spec.Weight,
		},
		Status: v1beta1.NodePoolStatus{
			Resources: provisioner.Status.Resources,
		},
		IsProvisioner: true,
	}
	if provisioner.Spec.TTLSecondsUntilExpired != nil {
		np.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(lo.Must(time.ParseDuration(fmt.Sprintf("%ds", lo.FromPtr[int64](provisioner.Spec.TTLSecondsUntilExpired)))))
	}
	if provisioner.Spec.Consolidation != nil && lo.FromPtr(provisioner.Spec.Consolidation.Enabled) {
		np.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
	} else if provisioner.Spec.TTLSecondsAfterEmpty != nil {
		np.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenEmpty
		np.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration(fmt.Sprintf("%ds", lo.FromPtr[int64](provisioner.Spec.TTLSecondsAfterEmpty)))))}
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

func Get(ctx context.Context, c client.Client, key Key) (*v1beta1.NodePool, error) {
	nodePool := &v1beta1.NodePool{}
	if err := c.Get(ctx, types.NamespacedName{Name: key.Name}, nodePool); err != nil {
		return nil, err
	}
	return nodePool, nil
}

func List(ctx context.Context, c client.Client, opts ...client.ListOption) (*v1beta1.NodePoolList, error) {
	nodePoolList := &v1beta1.NodePoolList{}
	if err := c.List(ctx, nodePoolList, opts...); err != nil {
		return nil, err
	}
	return nodePoolList, nil
}

func Patch(ctx context.Context, c client.Client, stored, nodePool *v1beta1.NodePool) error {
	return c.Patch(ctx, nodePool, client.MergeFrom(stored))
}

func PatchStatus(ctx context.Context, c client.Client, stored, nodePool *v1beta1.NodePool) error {
	return c.Status().Patch(ctx, nodePool, client.MergeFrom(stored))
}

func HashAnnotation(nodePool *v1beta1.NodePool) map[string]string {
	return map[string]string{v1beta1.NodePoolHashAnnotationKey: nodePool.Hash()}
}

func UpdateStatusCondition(ctx context.Context, c client.Client, np *v1beta1.NodePool, conditionType v1beta1.NodePoolConditionType, condition v1.ConditionStatus) {
	var found bool
	stored := np.DeepCopy()
	for i := range np.Status.Conditions {
		if np.Status.Conditions[i].Type == v1beta1.NodeClassConditionTypeReady {
			np.Status.Conditions[i].Status = condition
			found = true
			break
		}
	}

	if !found {
		np.Status.Conditions = append(np.Status.Conditions, v1beta1.NodePoolCondition{
			Type:   conditionType,
			Status: condition,
		})
	}

	if !equality.Semantic.DeepEqual(stored, np) {
		if err := PatchStatus(ctx, c, stored, np); err != nil {
			logging.FromContext(ctx).With("nodepool", np.Name).Errorf("unable to update nodeclass readiness into nodepool, %s", err)
		}
	}
	logging.FromContext(ctx).With("nodepool", np.Name).Info("updated nodeclass readiness into nodepool")
}
