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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VolumeUsage tracks volume limits on a per node basis.  The number of volumes that can be mounted varies by instance
// type. We need to be aware and track the mounted volume usage to inform our awareness of which pods can schedule to
// which nodes.
// +k8s:deepcopy-gen=true
//
//go:generate controller-gen object:headerFile="../../hack/boilerplate.go.txt" paths="."
type VolumeUsage struct {
	volumes    Volumes
	podVolumes map[types.NamespacedName]Volumes
	limits     map[string]int
}

func NewVolumeUsage() *VolumeUsage {
	return &VolumeUsage{
		volumes:    Volumes{},
		podVolumes: map[types.NamespacedName]Volumes{},
		limits:     map[string]int{},
	}
}

func (v *VolumeUsage) ExceedsLimits(vols Volumes) error {
	for k, volumes := range v.volumes.Union(vols) {
		if limit, hasLimit := v.limits[k]; hasLimit && len(volumes) > limit {
			return fmt.Errorf("would exceed volume limit for %s, %d > %d", k, len(volumes), limit)
		}
	}
	return nil
}

func (v *VolumeUsage) AddLimit(storageDriver string, value int) {
	v.limits[storageDriver] = value
}

func (v *VolumeUsage) Add(pod *v1.Pod, volumes Volumes) {
	v.podVolumes[client.ObjectKeyFromObject(pod)] = volumes
	v.volumes = v.volumes.Union(volumes)
}

func (v *VolumeUsage) DeletePod(key types.NamespacedName) {
	delete(v.podVolumes, key)
	// volume names could be duplicated, so we re-create our volumes
	v.volumes = Volumes{}
	for _, c := range v.podVolumes {
		v.volumes.Insert(c)
	}
}
