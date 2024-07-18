/*
Copyright The Kubernetes Authors.

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

package volumeattachment

import (
	"context"

	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/utils/pod"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodeutil "sigs.k8s.io/karpenter/pkg/utils/node"
	volumeutil "sigs.k8s.io/karpenter/pkg/utils/volume"
)

// FilterVolumeAttachments filters out volumeAttachments that should not block the termination of the passed node
func FilterVolumeAttachments(ctx context.Context, kubeClient client.Client, node *v1.Node, volumeAttachments []*storagev1.VolumeAttachment) ([]*storagev1.VolumeAttachment, error) {
	var filteredVolumeAttachments []*storagev1.VolumeAttachment
	// No need to filter empty volumeAttachments list
	if len(volumeAttachments) == 0 {
		return volumeAttachments, nil
	}
	// Filter out non-drain-able pods
	pods, err := nodeutil.GetPods(ctx, kubeClient, node)
	if err != nil {
		return nil, err
	}
	drainablePods := lo.Reject(pods, func(p *v1.Pod, _ int) bool {
		return pod.ToleratesDisruptionNoScheduleTaint(p)
	})
	// Filter out Multi-Attach volumes
	shouldNotFilterOutVolume := make(map[string]bool)
	for _, p := range drainablePods {
		for _, v := range p.Spec.Volumes {
			pvc, err := volumeutil.GetPersistentVolumeClaim(ctx, kubeClient, p, v)
			if err != nil {
				return nil, err
			}
			if pvc != nil {
				shouldNotFilterOutVolume[pvc.Spec.VolumeName] = CannotMultiAttach(*pvc)
			}
		}
	}
	for i := range volumeAttachments {
		pvName := volumeAttachments[i].Spec.Source.PersistentVolumeName
		if pvName != nil && shouldNotFilterOutVolume[*pvName] {
			filteredVolumeAttachments = append(filteredVolumeAttachments, volumeAttachments[i])
		}
	}
	return filteredVolumeAttachments, nil
}

// CannotMultiAttach returns true if the persistentVolumeClaim's underlying volume cannot be attached to multiple nodes
// i.e. its access mode is not ReadWriteOnce/ReadWriteOncePod
func CannotMultiAttach(pvc v1.PersistentVolumeClaim) bool {
	for _, accessMode := range pvc.Spec.AccessModes {
		if accessMode == v1.ReadWriteOnce || accessMode == v1.ReadWriteOncePod {
			return true
		}
	}
	return false
}
