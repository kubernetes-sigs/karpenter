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

	"knative.dev/pkg/logging"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VolumeUsage tracks volume limits on a per node basis.  The number of volumes that can be mounted varies by instance
// type. We need to be aware and track the mounted volume usage to inform our awareness of which pods can schedule to
// which nodes.
type VolumeUsage struct {
	volumes    volumes
	podVolumes map[types.NamespacedName]volumes
	kubeClient client.Client
}

type volumes map[string]sets.String

func (u volumes) Add(provisioner string, pvcID string) {
	existing, ok := u[provisioner]
	if !ok {
		existing = sets.NewString()
		u[provisioner] = existing
	}
	existing.Insert(pvcID)
}

func (u volumes) union(vol volumes) volumes {
	cp := volumes{}
	for k, v := range u {
		cp[k] = sets.NewString(v.List()...)
	}
	for k, v := range vol {
		existing, ok := cp[k]
		if !ok {
			existing = sets.NewString()
			cp[k] = existing
		}
		existing.Insert(v.List()...)
	}
	return cp
}

func (u volumes) insert(volumes volumes) {
	for k, v := range volumes {
		existing, ok := u[k]
		if !ok {
			existing = sets.NewString()
			u[k] = existing
		}
		existing.Insert(v.List()...)
	}
}

func (u volumes) copy() volumes {
	cp := volumes{}
	for k, v := range u {
		cp[k] = sets.NewString(v.List()...)
	}
	return cp
}

func NewVolumeLimits(kubeClient client.Client) *VolumeUsage {
	return &VolumeUsage{
		kubeClient: kubeClient,
		volumes:    volumes{},
		podVolumes: map[types.NamespacedName]volumes{},
	}
}

func (v *VolumeUsage) Add(ctx context.Context, pod *v1.Pod) {
	podVolumes, err := v.validate(ctx, pod)
	if err != nil {
		logging.FromContext(ctx).Errorf("inconsistent state error adding volume, %s, please file an issue", err)
	}
	v.podVolumes[client.ObjectKeyFromObject(pod)] = podVolumes
	v.volumes = v.volumes.union(podVolumes)
}

type VolumeCount map[string]int

// Exceeds returns true if the volume count exceeds the limits provided.  If there is no value for a storage provider, it
// is treated as unlimited.
func (c VolumeCount) Exceeds(limits VolumeCount) bool {
	for k, v := range c {
		limit, hasLimit := limits[k]
		if !hasLimit {
			continue
		}
		if v > limit {
			return true
		}
	}
	return false
}

// Fits returns true if the rhs 'fits' within the volume count.
func (c VolumeCount) Fits(rhs VolumeCount) bool {
	for k, v := range rhs {
		limit, hasLimit := c[k]
		if !hasLimit {
			continue
		}
		if v > limit {
			return false
		}
	}
	return true
}

func (v *VolumeUsage) Validate(ctx context.Context, pod *v1.Pod) (VolumeCount, error) {
	podVolumes, err := v.validate(ctx, pod)
	if err != nil {
		return nil, err
	}
	result := VolumeCount{}
	for k, v := range v.volumes.union(podVolumes) {
		result[k] += len(v)
	}
	return result, nil
}

func (v *VolumeUsage) validate(ctx context.Context, pod *v1.Pod) (volumes, error) {
	podPVCs := volumes{}
	for _, volume := range pod.Spec.Volumes {
		var pvcID string
		var storageClassName *string
		var volumeName string
		var pvc v1.PersistentVolumeClaim
		if volume.PersistentVolumeClaim != nil {
			if err := v.kubeClient.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: volume.PersistentVolumeClaim.ClaimName}, &pvc); err != nil {
				return nil, err
			}
			pvcID = fmt.Sprintf("%s/%s", pod.Namespace, volume.PersistentVolumeClaim.ClaimName)
			storageClassName = pvc.Spec.StorageClassName
			volumeName = pvc.Spec.VolumeName
		} else if volume.Ephemeral != nil {
			// generated name per https://kubernetes.io/docs/concepts/storage/ephemeral-volumes/#persistentvolumeclaim-naming
			pvcID = fmt.Sprintf("%s/%s-%s", pod.Namespace, pod.Name, volume.Name)
			storageClassName = volume.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName
			volumeName = volume.Ephemeral.VolumeClaimTemplate.Spec.VolumeName
		} else {
			continue
		}

		var driverName string
		var err error
		// We can track the volume usage by the CSI Driver name which is pulled from the storage class for dynamic
		// volumes, or if it's bound/static we can pull the volume name
		if volumeName != "" {
			driverName, err = v.driverFromVolume(ctx, volumeName)
			if err != nil {
				return nil, err
			}
		} else if storageClassName != nil && *storageClassName != "" {
			driverName, err = v.driverFromSC(ctx, storageClassName)
			if err != nil {
				return nil, err
			}
		}

		// might be a non-CSI driver, something we don't currently handle
		if driverName != "" {
			podPVCs.Add(driverName, pvcID)
		}
	}
	return podPVCs, nil
}

func (v *VolumeUsage) driverFromSC(ctx context.Context, storageClassName *string) (string, error) {
	var sc storagev1.StorageClass
	if err := v.kubeClient.Get(ctx, client.ObjectKey{Name: *storageClassName}, &sc); err != nil {
		return "", err
	}
	return sc.Provisioner, nil
}

func (v *VolumeUsage) driverFromVolume(ctx context.Context, volumeName string) (string, error) {
	var pv v1.PersistentVolume
	if err := v.kubeClient.Get(ctx, client.ObjectKey{Name: volumeName}, &pv); err != nil {
		return "", err
	}
	if pv.Spec.CSI != nil {
		return pv.Spec.CSI.Driver, nil
	}
	return "", nil
}

func (v *VolumeUsage) DeletePod(key types.NamespacedName) {
	delete(v.podVolumes, key)
	// volume names could be duplicated, so we re-create our volumes
	v.volumes = volumes{}
	for _, c := range v.podVolumes {
		v.volumes.insert(c)
	}
}

func (v *VolumeUsage) DeepCopy() *VolumeUsage {
	if v == nil {
		return nil
	}
	out := &VolumeUsage{}
	v.DeepCopyInto(out)
	return out
}

func (v *VolumeUsage) DeepCopyInto(out *VolumeUsage) {
	out.kubeClient = v.kubeClient
	out.volumes = v.volumes.copy()
	out.podVolumes = map[types.NamespacedName]volumes{}
	for k, v := range v.podVolumes {
		out.podVolumes[k] = v.copy()
	}
}
