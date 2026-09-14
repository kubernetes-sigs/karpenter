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
	// terminal records one-shot buffers the provisioner has latched terminal (Fulfilled or
	// Expired), keyed by namespace/name with the UID seen at latch time. UpdateEntry refuses to
	// rebuild pods for a buffer whose UID matches, closing the window in which the buffer
	// controller reconciles a copy that predates the terminal status patch and would otherwise
	// resurrect the virtual pods. A different UID means the buffer was deleted and recreated.
	terminal map[types.NamespacedName]types.UID
	// mutex guards capacityBufferToPods, terminal and warmed
	mutex sync.Mutex
	// warmed signals that capacityBufferToPods has been populated from the kube api state
	warmed bool
}

func NewVirtualPodCache(kubeClient client.Client) *Cache {
	return &Cache{
		kubeClient:           kubeClient,
		capacityBufferToPods: map[types.NamespacedName][]*corev1.Pod{},
		terminal:             map[types.NamespacedName]types.UID{},
	}
}

// UpdateEntry refreshes the cached virtual pods for a buffer using an
// already-resolved pod spec. The caller resolves
// the spec once to compute replicas and status, then passes it here so the cache
// doesn't re-fetch the same PodTemplate/workload
func (v *Cache) UpdateEntry(cb *autoscalingv1beta1.CapacityBuffer, spec corev1.PodTemplateSpec) {
	key := client.ObjectKeyFromObject(cb)
	v.mutex.Lock()
	defer v.mutex.Unlock()
	if uid, latched := v.terminal[key]; latched {
		if uid == cb.UID {
			// Terminal buffer: ignore this (possibly stale) copy and keep the entry evicted.
			delete(v.capacityBufferToPods, key)
			return
		}
		// Same name, new UID: the buffer was recreated, so the latch no longer applies.
		delete(v.terminal, key)
	}
	if !isBufferReadyForProvisioning(cb) {
		delete(v.capacityBufferToPods, key)
		return
	}
	v.capacityBufferToPods[key] = BuildVirtualPods(cb, spec)
}

// RemoveEntry drops a buffer's virtual pods and any terminal latch for it. Used when the buffer
// is deleted or can no longer be resolved.
func (v *Cache) RemoveEntry(key types.NamespacedName) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	delete(v.capacityBufferToPods, key)
	delete(v.terminal, key)
}

// MarkTerminal evicts a one-shot buffer's virtual pods and latches the buffer (by UID) so that a
// subsequent UpdateEntry from a stale copy cannot rebuild them. The latch is released by
// RemoveEntry, or automatically when a buffer with the same name but a new UID appears.
func (v *Cache) MarkTerminal(cb *autoscalingv1beta1.CapacityBuffer) {
	key := client.ObjectKeyFromObject(cb)
	v.mutex.Lock()
	defer v.mutex.Unlock()
	delete(v.capacityBufferToPods, key)
	v.terminal[key] = cb.UID
}

// Truncate keeps at most n of the buffer's cached virtual pods. The provisioner calls this when
// it advances a one-shot buffer's consumed count so the same scheduling cycle sees the shrunken
// remainder, without waiting for the buffer controller to observe the status patch and rebuild.
func (v *Cache) Truncate(key types.NamespacedName, n int) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	pods, ok := v.capacityBufferToPods[key]
	if !ok || len(pods) <= n {
		return
	}
	if n <= 0 {
		delete(v.capacityBufferToPods, key)
		return
	}
	v.capacityBufferToPods[key] = pods[:n]
}

// Get returns the cached virtual pods for a single buffer, hydrating the cache on first use.
// Like GetAll, the pods are not deep copied and MUST be treated as read-only.
func (v *Cache) Get(ctx context.Context, key types.NamespacedName) []*corev1.Pod {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	if err := v.ensureWarmed(ctx); err != nil {
		log.FromContext(ctx).Error(err, "failed to hydrate virtual pod cache")
		return nil
	}
	return v.capacityBufferToPods[key]
}

// ensureWarmed hydrates the cache once. The caller MUST hold v.mutex.
func (v *Cache) ensureWarmed(ctx context.Context) error {
	if v.warmed {
		return nil
	}
	if err := v.hydrateCache(ctx); err != nil {
		return err
	}
	// Only mark warmed after the cache is populated so a failed hydration retries on the next
	// call rather than serving an empty cache forever.
	v.warmed = true
	return nil
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
	if err := v.ensureWarmed(ctx); err != nil {
		log.FromContext(ctx).Error(err, "failed to hydrate virtual pod cache")
		return nil
	}
	ans := make([]*corev1.Pod, 0)
	for _, pods := range v.capacityBufferToPods {
		ans = append(ans, pods...)
	}
	return ans
}
