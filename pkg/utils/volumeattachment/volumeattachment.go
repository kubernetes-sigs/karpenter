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

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodeutil "sigs.k8s.io/karpenter/pkg/utils/node"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
	volumeutil "sigs.k8s.io/karpenter/pkg/utils/volume"
)

// FilterVolumeAttachments filters out volumeAttachments that should not block the termination of the passed node
func FilterVolumeAttachments(ctx context.Context, kubeClient client.Client, node *v1.Node, volumeAttachments []*storagev1.VolumeAttachment, clk clock.Clock) ([]*storagev1.VolumeAttachment, error) {
	// No need to filter empty volumeAttachments list
	if len(volumeAttachments) == 0 {
		return volumeAttachments, nil
	}
	// Filter out non-drain-able pods
	pods, err := nodeutil.GetPods(ctx, kubeClient, node)
	if err != nil {
		return nil, err
	}
	unDrainablePods := lo.Reject(pods, func(p *v1.Pod, _ int) bool {
		return pod.IsDrainable(p, clk)
	})
	// Filter out volumes
	shouldFilterOutVolume := make(map[string]bool)
	for _, p := range unDrainablePods {
		for _, v := range p.Spec.Volumes {
			pvc, err := volumeutil.GetPersistentVolumeClaim(ctx, kubeClient, p, v)
			if err != nil {
				continue
			}
			if pvc != nil {
				shouldFilterOutVolume[pvc.Spec.VolumeName] = true
			}
		}
	}
	filteredVolumeAttachments := lo.Reject(volumeAttachments, func(v *storagev1.VolumeAttachment, _ int) bool {
		pvName := v.Spec.Source.PersistentVolumeName
		return pvName == nil || shouldFilterOutVolume[*pvName]
	})
	return filteredVolumeAttachments, nil
}
