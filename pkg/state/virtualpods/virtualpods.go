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

package virtualpods

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

// Cache holds the virtual pods materialized for each CapacityBuffer
type Cache struct {
	// kubeClient is used to resolve buffer pod specs when hydrating the cache
	kubeClient client.Client
	// capacityBufferToPods maps a CapacityBuffer's namespace/name -> to a list of
	// virtual pods corresponding to that CapacityBuffer
	capacityBufferToPods map[types.NamespacedName][]*corev1.Pod
	// mutex guards capacityBufferToPods and warmed
	mutex sync.Mutex
	// warmed signals that capacityBufferToPods has been populated from the kube api state
	warmed bool
}

func NewVirtualPodCache(kubeClient client.Client) *Cache {
	return &Cache{
		kubeClient:           kubeClient,
		capacityBufferToPods: map[types.NamespacedName][]*corev1.Pod{},
	}
}

// UpdateEntry refreshes the cached virtual pods for a buffer using an
// already-resolved pod spec. The caller resolves
// the spec once to compute replicas and status, then passes it here so the cache
// doesn't re-fetch the same PodTemplate/workload
func (v *Cache) UpdateEntry(cb *autoscalingv1beta1.CapacityBuffer, spec corev1.PodTemplateSpec) {
	if !isBufferReadyForProvisioning(cb) {
		v.RemoveEntry(client.ObjectKeyFromObject(cb))
		return
	}
	podCache := BuildVirtualPods(cb, spec)
	v.mutex.Lock()
	defer v.mutex.Unlock()
	v.capacityBufferToPods[client.ObjectKeyFromObject(cb)] = podCache
}

func (v *Cache) RemoveEntry(key types.NamespacedName) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	delete(v.capacityBufferToPods, key)
}

// hydrateCache performs the one-time lazy hydration of the cache. The caller
// MUST hold v.mutex.
func (v *Cache) hydrateCache(ctx context.Context) error {
	buffers, err := listBuffersReadyForProvisioning(ctx, v.kubeClient)
	if err != nil {
		return err
	}

	newMap := make(map[types.NamespacedName][]*corev1.Pod)
	for _, cb := range buffers {
		spec, err := resolveVirtualPodSpec(ctx, v.kubeClient, cb)
		if err != nil {
			log.FromContext(ctx).WithValues("capacitybuffer", client.ObjectKeyFromObject(cb)).V(1).Info("skipping buffer", "reason", err.Error())
			continue
		}
		newMap[client.ObjectKeyFromObject(cb)] = BuildVirtualPods(cb, spec)
	}
	v.capacityBufferToPods = newMap
	return nil
}

// GetAll returns a snapshot of every cached virtual pod, hydrating the cache on
// first use. The pod objects are NOT deep copied, for performance
// Callers MUST treat the returned pods as read-only
func (v *Cache) GetAll(ctx context.Context) []*corev1.Pod {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	if !v.warmed {
		if err := v.hydrateCache(ctx); err != nil {
			log.FromContext(ctx).Error(err, "failed to hydrate virtual pod cache")
			return nil
		}
		// Only mark warmed after the cache is populated so a failed hydration
		// retries on the next call rather than serving an empty cache forever
		v.warmed = true
	}
	ans := make([]*corev1.Pod, 0)
	for _, pods := range v.capacityBufferToPods {
		ans = append(ans, pods...)
	}
	return ans
}
