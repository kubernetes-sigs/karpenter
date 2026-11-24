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

package capacitybuffer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("scalableRef watches", func() {
	var (
		watchCtx context.Context
		cancel   context.CancelFunc
	)

	BeforeEach(func() {
		watchCtx, cancel = context.WithCancel(context.Background())
	})

	AfterEach(func() {
		cancel()
	})

	It("should not open a watch when nothing references a workload", func() {
		opened := &atomic.Int32{}
		set := countingWatchSet(watchCtx, opened, func(context.Context, types.NamespacedName) {})
		Consistently(opened.Load, 200*time.Millisecond).Should(Equal(int32(0)))
		set.Sync(types.NamespacedName{Namespace: "default", Name: "buf"}, nil)
		set.Sync(types.NamespacedName{Namespace: "default", Name: "buf"}, &autoscalingv1beta1.ScalableRef{
			APIGroup: "apps", Kind: "DaemonSet", Name: "app",
		})
		set.Sync(types.NamespacedName{Namespace: "default", Name: "buf"}, &autoscalingv1beta1.ScalableRef{
			APIGroup: "batch", Kind: autoscalingv1beta1.KindDeployment, Name: "app",
		})
		Consistently(opened.Load, 200*time.Millisecond).Should(Equal(int32(0)))
	})

	It("should hold watches only while this process is the leader", func() {
		opened := &atomic.Int32{}
		active := &atomic.Int32{}
		set := newWorkloadWatchSet(cancelledContext(), func(ctx context.Context, _ workloadKey) (watch.Interface, error) {
			opened.Add(1)
			active.Add(1)
			go func() {
				<-ctx.Done()
				active.Add(-1)
			}()
			return newFakeWatch(), nil
		}, func(context.Context, types.NamespacedName) {})

		nn := types.NamespacedName{Namespace: "default", Name: "buf"}
		set.Sync(nn, deploymentRef("app"))
		Consistently(opened.Load, 200*time.Millisecond).Should(Equal(int32(0)))

		leaderCtx, lose := context.WithCancel(context.Background())
		returned := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(returned)
			Expect(set.Start(leaderCtx)).To(Succeed())
		}()
		Eventually(active.Load).Should(Equal(int32(1)))

		lose()
		Eventually(returned).Should(BeClosed())
		Eventually(active.Load).Should(Equal(int32(0)))

		leaderCtx, lose = context.WithCancel(context.Background())
		defer lose()
		go func() {
			defer GinkgoRecover()
			Expect(set.Start(leaderCtx)).To(Succeed())
		}()
		Eventually(opened.Load).Should(Equal(int32(2)))
		Eventually(active.Load).Should(Equal(int32(1)))
	})

	It("should share one watch across buffers that reference the same workload and stop it when the last one is gone", func() {
		opened := &atomic.Int32{}
		active := &atomic.Int32{}
		set := newWorkloadWatchSet(watchCtx, func(ctx context.Context, _ workloadKey) (watch.Interface, error) {
			opened.Add(1)
			active.Add(1)
			go func() {
				<-ctx.Done()
				active.Add(-1)
			}()
			return newFakeWatch(), nil
		}, func(context.Context, types.NamespacedName) {})

		ref := deploymentRef("app")
		a := types.NamespacedName{Namespace: "default", Name: "a"}
		b := types.NamespacedName{Namespace: "default", Name: "b"}
		set.Sync(a, ref)
		set.Sync(b, ref)
		// An empty API group is apps, so this is the same workload.
		set.Sync(types.NamespacedName{Namespace: "default", Name: "c"}, &autoscalingv1beta1.ScalableRef{
			Kind: autoscalingv1beta1.KindDeployment, Name: "app",
		})
		Eventually(opened.Load).Should(Equal(int32(1)))
		Eventually(active.Load).Should(Equal(int32(1)))

		set.Release(a)
		set.Sync(b, nil)
		Consistently(active.Load, 200*time.Millisecond).Should(Equal(int32(1)))

		set.Release(types.NamespacedName{Namespace: "default", Name: "c"})
		Eventually(active.Load).Should(Equal(int32(0)))
	})

	It("should replace the watch when a buffer changes targets", func() {
		var mu sync.Mutex
		var kinds []string
		set := newWorkloadWatchSet(watchCtx, func(_ context.Context, key workloadKey) (watch.Interface, error) {
			mu.Lock()
			kinds = append(kinds, key.kind)
			mu.Unlock()
			return newFakeWatch(), nil
		}, func(context.Context, types.NamespacedName) {})

		nn := types.NamespacedName{Namespace: "default", Name: "buf"}
		set.Sync(nn, deploymentRef("app"))
		// The watch starts asynchronously. Wait until it has opened before
		// retargeting, or the canceled watch can exit before open runs.
		Eventually(func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), kinds...)
		}).Should(Equal([]string{autoscalingv1beta1.KindDeployment}))
		set.Sync(nn, &autoscalingv1beta1.ScalableRef{APIGroup: "apps", Kind: autoscalingv1beta1.KindStatefulSet, Name: "app"})
		Eventually(func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), kinds...)
		}).Should(Equal([]string{autoscalingv1beta1.KindDeployment, autoscalingv1beta1.KindStatefulSet}))
	})

	It("should keep watches for the same name in different namespaces apart", func() {
		opened := &atomic.Int32{}
		set := countingWatchSet(watchCtx, opened, func(context.Context, types.NamespacedName) {})
		set.Sync(types.NamespacedName{Namespace: "a", Name: "buf"}, deploymentRef("app"))
		set.Sync(types.NamespacedName{Namespace: "b", Name: "buf"}, deploymentRef("app"))
		Eventually(opened.Load).Should(Equal(int32(2)))
	})

	It("should enqueue referencing buffers when the workload spec changes and skip status-only updates", func() {
		var mu sync.Mutex
		var got []types.NamespacedName
		opened := &atomic.Pointer[fakeWatch]{}
		set := newWorkloadWatchSet(watchCtx, func(context.Context, workloadKey) (watch.Interface, error) {
			fw := newFakeWatch()
			opened.Store(fw)
			return fw, nil
		}, func(_ context.Context, nn types.NamespacedName) {
			mu.Lock()
			got = append(got, nn)
			mu.Unlock()
		})

		a := types.NamespacedName{Namespace: "default", Name: "a"}
		b := types.NamespacedName{Namespace: "default", Name: "b"}
		set.Sync(a, deploymentRef("app"))
		set.Sync(b, deploymentRef("app"))
		Eventually(opened.Load).ShouldNot(BeNil())

		opened.Load().events <- watch.Event{Type: watch.Added, Object: workloadEvent(1)}
		Eventually(func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(got)
		}).Should(Equal(2))

		opened.Load().events <- watch.Event{Type: watch.Modified, Object: workloadEvent(1)}
		Consistently(func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(got)
		}, 200*time.Millisecond).Should(Equal(2))

		opened.Load().events <- watch.Event{Type: watch.Modified, Object: workloadEvent(2)}
		Eventually(func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(got)
		}).Should(Equal(4))
		mu.Lock()
		defer mu.Unlock()
		Expect(got).To(ConsistOf(a, b, a, b))
	})

	It("should enqueue when the watched workload is deleted", func() {
		var mu sync.Mutex
		var got int
		opened := &atomic.Pointer[fakeWatch]{}
		set := newWorkloadWatchSet(watchCtx, func(context.Context, workloadKey) (watch.Interface, error) {
			fw := newFakeWatch()
			opened.Store(fw)
			return fw, nil
		}, func(context.Context, types.NamespacedName) {
			mu.Lock()
			got++
			mu.Unlock()
		})
		set.Sync(types.NamespacedName{Namespace: "default", Name: "a"}, deploymentRef("app"))
		Eventually(opened.Load).ShouldNot(BeNil())

		opened.Load().events <- watch.Event{Type: watch.Added, Object: workloadEvent(3)}
		Eventually(func() int {
			mu.Lock()
			defer mu.Unlock()
			return got
		}).Should(Equal(1))
		opened.Load().events <- watch.Event{Type: watch.Deleted, Object: workloadEvent(3)}
		Eventually(func() int {
			mu.Lock()
			defer mu.Unlock()
			return got
		}).Should(Equal(2))
	})

	DescribeTable("should scope the watch to one object",
		func(kind string) {
			key := workloadKey{namespace: "default", group: appsv1.GroupName, kind: kind, name: "app"}
			opts := &client.ListOptions{}
			for _, opt := range workloadListOptions(key) {
				opt.ApplyToList(opts)
			}
			Expect(opts.Namespace).To(Equal("default"))
			Expect(opts.FieldSelector.String()).To(Equal("metadata.name=app"))
			Expect(key.metadataList().GroupVersionKind()).To(Equal(schema.GroupVersionKind{
				Group: appsv1.GroupName, Version: "v1", Kind: kind + "List",
			}))
		},
		Entry("Deployment", autoscalingv1beta1.KindDeployment),
		Entry("ReplicaSet", autoscalingv1beta1.KindReplicaSet),
		Entry("StatefulSet", autoscalingv1beta1.KindStatefulSet),
	)

	It("should deliver events only for the named workload", func() {
		target := test.Deployment(test.DeploymentOptions{ObjectMeta: metav1.ObjectMeta{Name: "cb-watch-target"}, Replicas: 1})
		other := test.Deployment(test.DeploymentOptions{ObjectMeta: metav1.ObjectMeta{Name: "cb-watch-other"}, Replicas: 1})
		ExpectApplied(ctx, env.Client, target, other)
		defer func() {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, target))).To(Succeed())
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, other))).To(Succeed())
		}()

		wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Client.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		key := workloadKey{namespace: target.Namespace, group: appsv1.GroupName, kind: autoscalingv1beta1.KindDeployment, name: target.Name}
		watcher, err := wc.Watch(watchCtx, key.metadataList(), workloadListOptions(key)...)
		Expect(err).NotTo(HaveOccurred())
		defer watcher.Stop()

		names := map[string]int{}
		var mu sync.Mutex
		go func() {
			defer GinkgoRecover()
			for {
				select {
				case <-watchCtx.Done():
					return
				case evt, ok := <-watcher.ResultChan():
					if !ok {
						return
					}
					if evt.Type != watch.Added && evt.Type != watch.Modified && evt.Type != watch.Deleted {
						continue
					}
					accessor, accErr := meta.Accessor(evt.Object)
					if accErr != nil || accessor.GetName() == "" {
						continue
					}
					mu.Lock()
					names[accessor.GetName()]++
					mu.Unlock()
				}
			}
		}()

		Eventually(func() int {
			mu.Lock()
			defer mu.Unlock()
			return names[target.Name]
		}).WithTimeout(20 * time.Second).Should(BeNumerically(">=", 1))
		mu.Lock()
		_, sawOther := names[other.Name]
		mu.Unlock()
		Expect(sawOther).To(BeFalse())

		target.Spec.Replicas = new(int32(2))
		other.Spec.Replicas = new(int32(2))
		ExpectApplied(ctx, env.Client, target, other)

		Eventually(func() int {
			mu.Lock()
			defer mu.Unlock()
			return names[target.Name]
		}).WithTimeout(20 * time.Second).Should(BeNumerically(">=", 2))
		Consistently(func() bool {
			mu.Lock()
			defer mu.Unlock()
			_, saw := names[other.Name]
			return saw
		}, time.Second).Should(BeFalse())
	})

	It("should resolve a scalableRef from the direct reader when the workload is not in the cached client", func() {
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "direct-reader-app", Namespace: "default"},
			Spec:       appsv1.DeploymentSpec{Replicas: new(int32(10))},
		}
		reader := &deploymentReader{obj: deploy}
		cbController.workloadReader = reader
		cb := &autoscalingv1beta1.CapacityBuffer{
			ObjectMeta: metav1.ObjectMeta{Name: "direct-reader-buffer", Namespace: "default"},
			Spec: autoscalingv1beta1.CapacityBufferSpec{
				ScalableRef: deploymentRef(deploy.Name),
				Replicas:    new(int32(3)),
			},
		}
		ExpectApplied(ctx, env.Client, cb)
		ExpectReconcileSucceeded(ctx, cbController, client.ObjectKeyFromObject(cb))

		Expect(reader.gets).To(BeNumerically(">=", 1))
		cb = ExpectExists(ctx, env.Client, cb)
		cond := findCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cb.Status.Replicas).ToNot(BeNil())
		Expect(*cb.Status.Replicas).To(Equal(int32(3)))
	})
})

func countingWatchSet(ctx context.Context, opened *atomic.Int32, notify func(context.Context, types.NamespacedName)) *workloadWatchSet {
	return newWorkloadWatchSet(ctx, func(ctx context.Context, _ workloadKey) (watch.Interface, error) {
		opened.Add(1)
		go func() { <-ctx.Done() }()
		return newFakeWatch(), nil
	}, notify)
}

func workloadEvent(generation int64) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default", Generation: generation}}
}

type fakeWatch struct {
	events chan watch.Event
	once   sync.Once
}

func newFakeWatch() *fakeWatch {
	return &fakeWatch{events: make(chan watch.Event, 8)}
}

func (f *fakeWatch) Stop() {
	f.once.Do(func() { close(f.events) })
}

func (f *fakeWatch) ResultChan() <-chan watch.Event { return f.events }

// deploymentReader serves one Deployment and counts Gets. List is unused.
type deploymentReader struct {
	obj  *appsv1.Deployment
	gets int
}

func (r *deploymentReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	r.gets++
	if key.Name != r.obj.Name || key.Namespace != r.obj.Namespace {
		return apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, key.Name)
	}
	deploy, ok := obj.(*appsv1.Deployment)
	if !ok {
		return fmt.Errorf("unexpected type %T", obj)
	}
	*deploy = *r.obj
	return nil
}

func (r *deploymentReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("unexpected list")
}
