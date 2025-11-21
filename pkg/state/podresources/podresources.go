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

package podresources

import (
	"sync"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// This class is primarily used to track the overall resource requests for pods in the cluster
// it is a proxy for the overall desired size of the cluster. It is flawed, and should be replaced
// with a superior desired resource count mechanism that handles the following cases:
//  1. Pod manager objects (replicasets, jobs) and their replacement workflows.
//  2. Standalone pods
//  3. IPPA

type PodResources struct {
	sync.RWMutex
	podMap map[string]corev1.ResourceList
	total  corev1.ResourceList
	count  int
}

func NewPodResources() *PodResources {
	return &PodResources{
		podMap: make(map[string]corev1.ResourceList),
		total:  nil,
		count:  0,
	}
}

func (pr *PodResources) UpdatePod(p *corev1.Pod) {
	pr.Lock()
	defer pr.Unlock()

	podKey := client.ObjectKeyFromObject(p).String()
	rl, exists := pr.podMap[podKey]
	totalResources := corev1.ResourceList{}

	for _, r := range p.Spec.Containers {
		totalResources = resources.MergeInto(totalResources, r.Resources.Requests)
	}
	if !exists {
		pr.podMap[podKey] = totalResources
		pr.total = resources.MergeInto(pr.total, totalResources)
		pr.count++
		return
	} else if !resources.Eql(rl, totalResources) {
		resources.SubtractFrom(pr.total, rl)
		pr.total = resources.MergeInto(pr.total, totalResources)
	}
}

func (pr *PodResources) DeletePod(p *corev1.Pod) {
	pr.Lock()
	defer pr.Unlock()

	podKey := client.ObjectKeyFromObject(p).String()
	rl, exists := pr.podMap[podKey]

	if !exists {
		return
	}
	pr.count--
	resources.SubtractFrom(pr.total, rl)
	delete(pr.podMap, podKey)
}

func (pr *PodResources) GetTotalPodResourceRequests() *corev1.ResourceList {
	pr.Lock()
	defer pr.Unlock()
	return lo.ToPtr(pr.total.DeepCopy())
}

func (pr *PodResources) GetTotalPodCount() int {
	pr.RLock()
	defer pr.RUnlock()
	return pr.count
}
