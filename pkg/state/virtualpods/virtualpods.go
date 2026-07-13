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
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

// Cache holds the virtual (placeholder) pods materialized for each CapacityBuffer
// so the provisioner can schedule against buffer capacity.
type Cache struct {
	// kubeClient is used to resolve buffer pod specs when hydrating the cache.
	kubeClient client.Client
	// capacityBufferToPods maps a capacity buffer identifier (name/namespace) -> to a list of virtual pods
	capacityBufferToPods map[string][]*corev1.Pod
	// mutex protects capacityBufferToPods
	mutex sync.RWMutex
}

// UpdateEntry refreshes the cached virtual pods for a buffer using an
// already-resolved pod spec. The caller (the capacity buffer controller) resolves
// the spec once to compute replicas and status, then passes it here so the cache
// doesn't re-fetch the same PodTemplate/workload.
func (v *Cache) UpdateEntry(cb *autoscalingv1beta1.CapacityBuffer, spec corev1.PodSpec) {
	if !isBufferReadyForProvisioning(cb) {
		v.RemoveEntry(cb.Namespace, cb.Name)
		return
	}
	podCache := BuildVirtualPods(cb, spec)
	v.mutex.Lock()
	defer v.mutex.Unlock()
	v.capacityBufferToPods[bufferKey(cb.GetNamespace(), cb.GetName())] = podCache
}

func (v *Cache) RemoveEntry(namespace, name string) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	delete(v.capacityBufferToPods, bufferKey(namespace, name))
}

func (v *Cache) HydrateCache(ctx context.Context) error {
	buffers, err := listBuffersReadyForProvisioning(ctx, v.kubeClient)
	if err != nil {
		return err
	}

	newMap := make(map[string][]*corev1.Pod)

	for _, cb := range buffers {
		spec, err := resolveVirtualPodSpec(ctx, v.kubeClient, cb)
		if err != nil {
			log.FromContext(ctx).WithValues("capacitybuffer", client.ObjectKeyFromObject(cb)).V(1).Info("skipping buffer", "reason", err.Error())
			continue
		}
		newMap[bufferKey(cb.GetNamespace(), cb.GetName())] = BuildVirtualPods(cb, spec)
	}
	v.mutex.Lock()
	defer v.mutex.Unlock()
	v.capacityBufferToPods = newMap
	return nil
}

// GetAll returns a snapshot of every cached virtual pod. The returned pod
// objects are NOT deep copied, for performance; callers MUST treat them as
// read-only and never mutate them.
func (v *Cache) GetAll() []*corev1.Pod {
	v.mutex.RLock()
	defer v.mutex.RUnlock()
	ans := make([]*corev1.Pod, 0)
	for _, pods := range v.capacityBufferToPods {
		ans = append(ans, pods...)
	}
	return ans
}

func NewVirtualPodCache(kubeClient client.Client) *Cache {
	return &Cache{
		kubeClient:           kubeClient,
		capacityBufferToPods: map[string][]*corev1.Pod{},
		mutex:                sync.RWMutex{},
	}
}

// CacheWarmer hydrates the virtual pod cache during the manager's warmup phase.
// controller-runtime runs warmup runnables after informer caches have synced but
// before any leader-elected runnable, and it blocks until warmup completes.
// Registering the warmer therefore guarantees the cache is populated before the
// provisioner or disruption controllers ever read from it, closing the
// provisioning cold-start window without racing controller reconciles.
type CacheWarmer struct {
	cache *Cache
}

var _ interface {
	Warmup(context.Context) error
} = (*CacheWarmer)(nil)

// NewCacheWarmer returns a manager.Runnable that hydrates the given cache during
// manager warmup.
func NewCacheWarmer(cache *Cache) *CacheWarmer {
	return &CacheWarmer{cache: cache}
}

func (w *CacheWarmer) Warmup(ctx context.Context) error {
	if err := w.cache.HydrateCache(ctx); err != nil {
		log.FromContext(ctx).Error(err, "failed to hydrate virtual pod cache during warmup")
	}
	return nil
}

// Start is a no-op. The cache is populated in Warmup; Start exists only to
// satisfy manager.Runnable so the warmer can be registered with the manager.
func (w *CacheWarmer) Start(_ context.Context) error {
	return nil
}

// bufferKey builds the map key that identifies a CapacityBuffer's cache entry.
func bufferKey(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}
