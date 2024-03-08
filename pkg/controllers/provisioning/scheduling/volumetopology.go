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
	"context"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/scheduling"
)

func NewVolumeTopology(kubeClient client.Client) *VolumeTopology {
	return &VolumeTopology{kubeClient: kubeClient}
}

type VolumeTopology struct {
	kubeClient client.Client
}

func (v *VolumeTopology) Inject(ctx context.Context, pod *v1.Pod) error {
	var requirements []v1.NodeSelectorRequirement
	for _, volume := range pod.Spec.Volumes {
		req, err := v.getRequirements(ctx, pod, volume)
		if err != nil {
			return err
		}
		requirements = append(requirements, req...)
	}
	if len(requirements) == 0 {
		return nil
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &v1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &v1.NodeAffinity{}
	}
	if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &v1.NodeSelector{}
	}
	if len(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = []v1.NodeSelectorTerm{{}}
	}

	// We add our volume topology zonal requirement to every node selector term.  This causes it to be AND'd with every existing
	// requirement so that relaxation won't remove our volume requirement.
	for i := 0; i < len(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms); i++ {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i].MatchExpressions = append(
			pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i].MatchExpressions, requirements...)
	}

	logging.FromContext(ctx).
		With("pod", client.ObjectKeyFromObject(pod)).
		Debugf("adding requirements derived from pod volumes, %s", requirements)
	return nil
}

func (v *VolumeTopology) getRequirements(ctx context.Context, pod *v1.Pod, volume v1.Volume) ([]v1.NodeSelectorRequirement, error) {
	defaultStorageClassName, err := scheduling.DiscoverDefaultStorageClassName(ctx, v.kubeClient)
	if err != nil {
		return nil, fmt.Errorf("discovering default storage class, %w", err)
	}

	// Get VolumeName and StorageClass name from PVC
	pvc := &v1.PersistentVolumeClaim{}
	switch {
	case volume.PersistentVolumeClaim != nil:
		if err = v.kubeClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: volume.PersistentVolumeClaim.ClaimName}, pvc); err != nil {
			return nil, fmt.Errorf("discovering persistent volume claim, %w", err)
		}
	case volume.Ephemeral != nil:
		// generated name per https://kubernetes.io/docs/concepts/storage/ephemeral-volumes/#persistentvolumeclaim-naming
		if err = v.kubeClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: fmt.Sprintf("%s-%s", pod.Name, volume.Name)}, pvc); err != nil {
			return nil, fmt.Errorf("discovering persistent volume claim for ephemeral volume, %w", err)
		}
	default:
		return nil, nil
	}
	storageClassName := lo.FromPtr(pvc.Spec.StorageClassName)
	if storageClassName == "" {
		storageClassName = defaultStorageClassName
	}
	// Persistent Volume Requirements
	if pvc.Spec.VolumeName != "" {
		requirements, err := v.getPersistentVolumeRequirements(ctx, pod, pvc.Spec.VolumeName)
		if err != nil {
			return nil, fmt.Errorf("getting existing requirements, %w", err)
		}
		return requirements, nil
	}
	// Storage Class Requirements
	if storageClassName != "" {
		requirements, err := v.getStorageClassRequirements(ctx, storageClassName)
		if err != nil {
			return nil, err
		}
		return requirements, nil
	}
	return nil, nil
}

func (v *VolumeTopology) getStorageClassRequirements(ctx context.Context, storageClassName string) ([]v1.NodeSelectorRequirement, error) {
	storageClass := &storagev1.StorageClass{}
	if err := v.kubeClient.Get(ctx, types.NamespacedName{Name: storageClassName}, storageClass); err != nil {
		return nil, fmt.Errorf("getting storage class %q, %w", storageClassName, err)
	}
	var requirements []v1.NodeSelectorRequirement
	if len(storageClass.AllowedTopologies) > 0 {
		// Terms are ORed, only use the first term
		for _, requirement := range storageClass.AllowedTopologies[0].MatchLabelExpressions {
			requirements = append(requirements, v1.NodeSelectorRequirement{Key: requirement.Key, Operator: v1.NodeSelectorOpIn, Values: requirement.Values})
		}
	}
	return requirements, nil
}

func (v *VolumeTopology) getPersistentVolumeRequirements(ctx context.Context, pod *v1.Pod, volumeName string) ([]v1.NodeSelectorRequirement, error) {
	pv := &v1.PersistentVolume{}
	if err := v.kubeClient.Get(ctx, types.NamespacedName{Name: volumeName, Namespace: pod.Namespace}, pv); err != nil {
		return nil, fmt.Errorf("getting persistent volume %q, %w", volumeName, err)
	}
	if pv.Spec.NodeAffinity == nil {
		return nil, nil
	}
	if pv.Spec.NodeAffinity.Required == nil {
		return nil, nil
	}
	var requirements []v1.NodeSelectorRequirement
	if len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms) > 0 {
		// Terms are ORed, only use the first term
		requirements = pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions
		// If we are using a Local volume or a HostPath volume, then we should ignore the Hostname affinity
		// on it because re-scheduling this pod to a new node means not using the same Hostname affinity that we currently have
		if pv.Spec.Local != nil || pv.Spec.HostPath != nil {
			requirements = lo.Reject(requirements, func(n v1.NodeSelectorRequirement, _ int) bool {
				return n.Key == v1.LabelHostname
			})
		}
	}
	return requirements, nil
}

func (v *VolumeTopology) getPersistentVolumeClaim(ctx context.Context, pod *v1.Pod, volume v1.Volume) (*v1.PersistentVolumeClaim, error) {
	if volume.PersistentVolumeClaim == nil {
		return nil, nil
	}
	pvc := &v1.PersistentVolumeClaim{}
	if err := v.kubeClient.Get(ctx, types.NamespacedName{Name: volume.PersistentVolumeClaim.ClaimName, Namespace: pod.Namespace}, pvc); err != nil {
		return nil, fmt.Errorf("getting persistent volume claim %q, %w", volume.PersistentVolumeClaim.ClaimName, err)
	}
	return pvc, nil
}

// ValidatePersistentVolumeClaims returns an error if the pod doesn't appear to be valid with respect to
// PVCs (e.g. the PVC is not found or references an unknown storage class).
func (v *VolumeTopology) ValidatePersistentVolumeClaims(ctx context.Context, pod *v1.Pod) error {
	for _, volume := range pod.Spec.Volumes {
		var storageClassName *string
		var volumeName string
		if volume.PersistentVolumeClaim != nil {
			// validate the PVC if it exists
			pvc, err := v.getPersistentVolumeClaim(ctx, pod, volume)
			if err != nil {
				return err
			}
			// may not have a PVC
			if pvc == nil {
				continue
			}

			storageClassName = pvc.Spec.StorageClassName
			volumeName = pvc.Spec.VolumeName
		} else if volume.Ephemeral != nil {
			storageClassName = volume.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName
		}

		if err := v.validateStorageClass(ctx, storageClassName); err != nil {
			return err
		}
		if err := v.validateVolume(ctx, volumeName); err != nil {
			return err
		}
	}
	return nil
}

func (v *VolumeTopology) validateVolume(ctx context.Context, volumeName string) error {
	// we have a volume name, so ensure that it exists
	if volumeName != "" {
		pv := &v1.PersistentVolume{}
		if err := v.kubeClient.Get(ctx, types.NamespacedName{Name: volumeName}, pv); err != nil {
			return err
		}
	}
	return nil
}

func (v *VolumeTopology) validateStorageClass(ctx context.Context, storageClassName *string) error {
	// we have a storage class name, so ensure that it exists
	if ptr.StringValue(storageClassName) != "" {
		storageClass := &storagev1.StorageClass{}
		if err := v.kubeClient.Get(ctx, types.NamespacedName{Name: ptr.StringValue(storageClassName)}, storageClass); err != nil {
			return err
		}
	}
	return nil
}
