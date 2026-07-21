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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

var _ = Describe("VirtualPodCache", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("GetAll", func() {
		It("should return an empty slice for a fresh cache with no buffers", func() {
			cache := NewVirtualPodCache(fakeClient())
			Expect(cache.GetAll(ctx)).To(BeEmpty())
		})

		It("should lazily hydrate from the cluster on the first call", func() {
			web := readyBuffer("web", 3)
			api := readyBuffer("api", 2)
			cache := NewVirtualPodCache(fakeClient(web, api, podTemplateFor(web), podTemplateFor(api)))

			// Nothing has populated the cache yet; the first GetAll must hydrate it.
			Expect(cache.GetAll(ctx)).To(HaveLen(5))
		})

		It("should hydrate at most once", func() {
			web := readyBuffer("web", 3)
			cache := NewVirtualPodCache(fakeClient(web, podTemplateFor(web)))
			Expect(cache.GetAll(ctx)).To(HaveLen(3))

			// Point the cache at an empty cluster. A second GetAll must NOT
			// re-list, so it keeps serving the already-hydrated pods.
			cache.kubeClient = fakeClient()
			Expect(cache.GetAll(ctx)).To(HaveLen(3))
		})

		It("should retry hydration if the first attempt found nothing due to a list error", func() {
			// A cache whose first hydration fails leaves warmed=false so the next
			// call retries. Simulate recovery by swapping in a populated client.
			cache := NewVirtualPodCache(fakeClient())
			Expect(cache.GetAll(ctx)).To(BeEmpty())

			web := readyBuffer("web", 3)
			cache.kubeClient = fakeClient(web, podTemplateFor(web))
			// An empty cluster is not an error, so warmed stayed true and we do
			// NOT re-hydrate here; the cache remains empty.
			Expect(cache.GetAll(ctx)).To(BeEmpty())
		})

		It("should never hand a concurrent caller an empty cache while hydrating", func() {
			web := readyBuffer("web", 3)
			api := readyBuffer("api", 2)
			cache := NewVirtualPodCache(fakeClient(web, api, podTemplateFor(web), podTemplateFor(api)))

			// Fire many concurrent GetAll calls against the cold cache. Every
			// caller must block until the one-time hydration completes and see
			// the fully populated result — never a partial or empty slice.
			const callers = 50
			results := make([]int, callers)
			var wg sync.WaitGroup
			wg.Add(callers)
			for i := 0; i < callers; i++ {
				go func(idx int) {
					defer wg.Done()
					results[idx] = len(cache.GetAll(ctx))
				}(i)
			}
			wg.Wait()

			for _, got := range results {
				Expect(got).To(Equal(5))
			}
		})
	})

	Describe("UpdateEntry", func() {
		It("should populate the cache with the buffer's virtual pods", func() {
			cb := readyBuffer("web", 3)
			cache := NewVirtualPodCache(fakeClient(podTemplateFor(cb)))
			// Isolate UpdateEntry from lazy hydration: treat the cache as already
			// warmed so GetAll won't re-list the (buffer-less) cluster.
			cache.warmed.Store(true)

			resolveAndUpdate(ctx, cache, cb)

			pods := cache.GetAll(ctx)
			Expect(pods).To(HaveLen(3))
			for _, p := range pods {
				Expect(p.Labels[autoscalingv1beta1.BufferNameLabel]).To(Equal("web"))
			}
		})

		It("should replace an existing entry when called again", func() {
			cb := readyBuffer("web", 3)
			cache := NewVirtualPodCache(fakeClient(podTemplateFor(cb)))
			cache.warmed.Store(true)
			resolveAndUpdate(ctx, cache, cb)
			Expect(cache.GetAll(ctx)).To(HaveLen(3))

			// Reduce replicas and update again; the entry should be replaced, not appended.
			cb.Status.Replicas = ptr(int32(1))
			resolveAndUpdate(ctx, cache, cb)
			Expect(cache.GetAll(ctx)).To(HaveLen(1))
		})

		It("should remove the entry when the buffer is no longer ready for provisioning", func() {
			cb := readyBuffer("web", 3)
			cache := NewVirtualPodCache(fakeClient(podTemplateFor(cb)))
			cache.warmed.Store(true)
			resolveAndUpdate(ctx, cache, cb)
			Expect(cache.GetAll(ctx)).To(HaveLen(3))

			// Flip the buffer to not-ready and update. UpdateEntry drops the entry
			// regardless of the spec it is handed.
			cb.Status.Conditions[0].Status = metav1.ConditionFalse
			cache.UpdateEntry(cb, corev1.PodSpec{})
			Expect(cache.GetAll(ctx)).To(BeEmpty())
		})

		It("should build pods for scalableRef buffers", func() {
			cb := readyScalableRefBuffer("scalable", 2)
			cache := NewVirtualPodCache(fakeClient(deploymentFor(cb)))
			cache.warmed.Store(true)

			resolveAndUpdate(ctx, cache, cb)
			Expect(cache.GetAll(ctx)).To(HaveLen(2))
		})
	})

	Describe("RemoveEntry", func() {
		It("should remove the entry for the given namespace/name", func() {
			cb := readyBuffer("web", 3)
			cache := NewVirtualPodCache(fakeClient(podTemplateFor(cb)))
			cache.warmed.Store(true)
			resolveAndUpdate(ctx, cache, cb)
			Expect(cache.GetAll(ctx)).To(HaveLen(3))

			cache.RemoveEntry(cb.Namespace, cb.Name)
			Expect(cache.GetAll(ctx)).To(BeEmpty())
		})

		It("should be a no-op for an unknown entry", func() {
			cache := NewVirtualPodCache(fakeClient())
			cache.warmed.Store(true)
			cache.RemoveEntry("default", "does-not-exist")
			Expect(cache.GetAll(ctx)).To(BeEmpty())
		})
	})

	Describe("hydrateCache", func() {
		It("should populate the cache from all ready buffers in the cluster", func() {
			web := readyBuffer("web", 3)
			api := readyBuffer("api", 2)
			cache := NewVirtualPodCache(fakeClient(web, api, podTemplateFor(web), podTemplateFor(api)))

			Expect(cache.hydrateCache(ctx)).To(Succeed())
			cache.warmed.Store(true)
			Expect(cache.GetAll(ctx)).To(HaveLen(5))
		})

		It("should skip buffers that are not ready for provisioning", func() {
			ready := readyBuffer("web", 3)
			notReady := readyBuffer("api", 2)
			notReady.Status.Conditions[0].Status = metav1.ConditionFalse
			cache := NewVirtualPodCache(fakeClient(ready, notReady, podTemplateFor(ready), podTemplateFor(notReady)))

			Expect(cache.hydrateCache(ctx)).To(Succeed())
			cache.warmed.Store(true)
			pods := cache.GetAll(ctx)
			Expect(pods).To(HaveLen(3))
			for _, p := range pods {
				Expect(p.Labels[autoscalingv1beta1.BufferNameLabel]).To(Equal("web"))
			}
		})

		It("should skip buffers whose pod spec cannot be resolved", func() {
			resolvable := readyBuffer("web", 3)
			unresolvable := readyBuffer("api", 2)
			// Only create the template for the resolvable buffer.
			cache := NewVirtualPodCache(fakeClient(resolvable, unresolvable, podTemplateFor(resolvable)))

			Expect(cache.hydrateCache(ctx)).To(Succeed())
			cache.warmed.Store(true)
			Expect(cache.GetAll(ctx)).To(HaveLen(3))
		})

		It("should replace previously cached entries", func() {
			web := readyBuffer("web", 3)
			cache := NewVirtualPodCache(fakeClient(web, podTemplateFor(web)))
			cache.warmed.Store(true)
			resolveAndUpdate(ctx, cache, web)
			Expect(cache.GetAll(ctx)).To(HaveLen(3))

			// Hydrate against a cluster that only has a different buffer.
			api := readyBuffer("api", 2)
			cache.kubeClient = fakeClient(api, podTemplateFor(api))
			Expect(cache.hydrateCache(ctx)).To(Succeed())

			pods := cache.GetAll(ctx)
			Expect(pods).To(HaveLen(2))
			for _, p := range pods {
				Expect(p.Labels[autoscalingv1beta1.BufferNameLabel]).To(Equal("api"))
			}
		})

		It("should return an empty cache when there are no buffers", func() {
			cache := NewVirtualPodCache(fakeClient())
			Expect(cache.hydrateCache(ctx)).To(Succeed())
			cache.warmed.Store(true)
			Expect(cache.GetAll(ctx)).To(BeEmpty())
		})
	})

	Describe("concurrency", func() {
		It("should safely handle concurrent reads and writes", func() {
			cb := readyBuffer("web", 3)
			cache := NewVirtualPodCache(fakeClient(podTemplateFor(cb)))
			// Treat the cache as warmed so concurrent GetAll calls read the map
			// rather than racing to lazily hydrate an empty cluster.
			cache.warmed.Store(true)
			spec, err := resolveVirtualPodSpec(ctx, cache.kubeClient, cb)
			Expect(err).ToNot(HaveOccurred())

			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(2)
				go func() {
					defer wg.Done()
					cache.UpdateEntry(cb, spec)
				}()
				go func() {
					defer wg.Done()
					_ = cache.GetAll(ctx)
				}()
			}
			wg.Wait()

			Expect(cache.GetAll(ctx)).To(HaveLen(3))
		})
	})
})

func fakeClient(objs ...client.Object) client.Client {
	return fakecr.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
}

// resolveAndUpdate mirrors the controller flow: resolve the pod spec once (using
// the cache's own client), then hand it to the cache. Resolution is expected to
// succeed.
func resolveAndUpdate(ctx context.Context, cache *Cache, cb *autoscalingv1beta1.CapacityBuffer) {
	spec, err := resolveVirtualPodSpec(ctx, cache.kubeClient, cb)
	Expect(err).ToNot(HaveOccurred())
	cache.UpdateEntry(cb, spec)
}

// podTemplateFor builds the PodTemplate referenced by a podTemplateRef buffer
// created via readyBuffer (which uses "<name>-template").
func podTemplateFor(cb *autoscalingv1beta1.CapacityBuffer) *corev1.PodTemplate {
	return &corev1.PodTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cb.Spec.PodTemplateRef.Name,
			Namespace: cb.Namespace,
		},
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "pause:v1"}},
			},
		},
	}
}

// deploymentFor builds the Deployment referenced by a scalableRef buffer
// created via readyScalableRefBuffer (which uses "<name>-deploy").
func deploymentFor(cb *autoscalingv1beta1.CapacityBuffer) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cb.Spec.ScalableRef.Name,
			Namespace: cb.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(10)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
				},
			},
		},
	}
}

func ptr[T any](v T) *T {
	return &v
}
