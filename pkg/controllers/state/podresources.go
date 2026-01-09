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

package state

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// This class is primarily used to track the overall resource requests for pods in the cluster
// it is a proxy for the overall desired size of the cluster. It is flawed, and should be replaced
// with a superior desired resource count mechanism that handles the following cases:
//  1. Pod manager objects (replicasets, jobs) and their replacement workflows.
//  2. Standalone pods
//  3. In Place Pod Autoscaling

type PodResources struct {
	sync.RWMutex
	podMap map[types.NamespacedName]corev1.ResourceList
	total  corev1.ResourceList
}

func NewPodResources() PodResources {
	return PodResources{
		podMap: make(map[types.NamespacedName]corev1.ResourceList),
		total:  corev1.ResourceList{},
	}
}

func (pr *PodResources) UpdatePod(p *corev1.Pod) {
	pr.Lock()
	defer pr.Unlock()

	podKey := client.ObjectKeyFromObject(p)
	rl, exists := pr.podMap[podKey]

	totalResources := resources.RequestsForPods(p)
	pr.podMap[podKey] = totalResources

	if !exists {
		pr.total = resources.MergeInto(pr.total, totalResources)
		return
	} else if !resources.Eql(rl, totalResources) {
		resources.SubtractFrom(pr.total, rl)
		pr.total = resources.MergeInto(pr.total, totalResources)
	}
}

func (pr *PodResources) DeletePod(p types.NamespacedName) {
	pr.Lock()
	defer pr.Unlock()

	rl, exists := pr.podMap[p]

	if !exists {
		return
	}
	resources.SubtractFrom(pr.total, rl)
	delete(pr.podMap, p)
}

func (pr *PodResources) GetTotalPodResourceRequests() corev1.ResourceList {
	pr.Lock()
	defer pr.Unlock()
	return pr.total.DeepCopy()
}

func (pr *PodResources) GetTotalPodCount() int {
	pr.RLock()
	defer pr.RUnlock()
	return len(pr.podMap)
}
