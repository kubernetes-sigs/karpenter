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
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

const (
	watchRetryInitial = time.Second
	watchRetryMax     = 30 * time.Second
	// A watch that stays up this long is treated as healthy, so the next
	// reconnect starts the backoff over.
	watchStableFor = 10 * time.Second
)

// workloadKey is one scalableRef target. namespace is the buffer's namespace:
// scalableRef is same-namespace.
type workloadKey struct {
	namespace string
	group     string
	kind      string
	name      string
}

// workloadWatchSet opens one API watch per referenced workload.
//
// A controller-runtime Watch on Deployment, ReplicaSet, or StatefulSet starts a
// cluster-wide informer and keeps every object of that kind. Clusters with tens
// of thousands of historical ReplicaSets spend multiple gigabytes on that cache
// even when no CapacityBuffer exists. Each watch here is a single-object
// metadata watch (field selector metadata.name) and stores nothing. The
// controller reads that one object from the API server when it reconciles.
type workloadWatchSet struct {
	// name is the controller name stamped on watch logs. Empty in tests.
	name string
	// leaderCtx is canceled when this process is not the leader. Watches are
	// children of it, so losing leadership closes them. Tests pass a live
	// context; Register passes an already-canceled one and Start replaces it.
	leaderCtx context.Context
	open      func(context.Context, workloadKey) (watch.Interface, error)
	notify    func(context.Context, types.NamespacedName)

	mu sync.Mutex
	// byBuffer is the workload a buffer currently holds a reference on.
	byBuffer map[types.NamespacedName]workloadKey
	// subscribers are the buffers whose scalableRef names that workload.
	// Kept across leadership changes so the next term can reopen watches
	// before the buffers reconcile again.
	subscribers map[workloadKey]map[types.NamespacedName]struct{}
	cancel      map[workloadKey]context.CancelFunc
}

func newWorkloadWatchSet(
	ctx context.Context,
	open func(context.Context, workloadKey) (watch.Interface, error),
	notify func(context.Context, types.NamespacedName),
) *workloadWatchSet {
	return &workloadWatchSet{
		leaderCtx:   ctx,
		open:        open,
		notify:      notify,
		byBuffer:    map[types.NamespacedName]workloadKey{},
		subscribers: map[workloadKey]map[types.NamespacedName]struct{}{},
		cancel:      map[workloadKey]context.CancelFunc{},
	}
}

// Start runs while this process is the leader. Losing leadership closes every
// workload watch; the next term opens them again.
func (s *workloadWatchSet) Start(ctx context.Context) error {
	if s.name != "" {
		ctx = injection.WithControllerName(ctx, s.name)
	}
	s.mu.Lock()
	s.leaderCtx = ctx
	s.launchLocked()
	s.mu.Unlock()

	<-ctx.Done()

	s.mu.Lock()
	defer s.mu.Unlock()
	// A newer leadership term may already have installed its own context.
	if s.leaderCtx != ctx {
		return nil
	}
	s.stopLocked()
	return nil
}

// NeedLeaderElection reports that watches run only on the leader. A standby
// replica must not hold them.
func (s *workloadWatchSet) NeedLeaderElection() bool { return true }

func (s *workloadWatchSet) launchLocked() {
	for key, subs := range s.subscribers {
		if len(subs) == 0 {
			continue
		}
		// A watch recorded here belongs to the previous term. Cancel it and
		// open a new one against the current leader context.
		if cancel, ok := s.cancel[key]; ok {
			cancel()
			delete(s.cancel, key)
		}
		s.startWatchLocked(key)
	}
}

func (s *workloadWatchSet) startWatchLocked(key workloadKey) {
	if s.leaderCtx == nil || s.leaderCtx.Err() != nil {
		return
	}
	watchCtx, cancel := context.WithCancel(s.leaderCtx)
	s.cancel[key] = cancel
	go func() {
		// Release the derived context when the watch goroutine exits. stopLocked and
		// launchLocked also call cancel, and CancelFunc is safe to call more than once.
		defer cancel()
		s.run(watchCtx, key)
	}()
}

func (s *workloadWatchSet) stopLocked() {
	for key, cancel := range s.cancel {
		cancel()
		delete(s.cancel, key)
	}
	s.leaderCtx = nil
}

// Sync makes buffer the only workload watch held for its current scalableRef.
// A nil or unsupported ref drops any watch the buffer held.
func (s *workloadWatchSet) Sync(buffer types.NamespacedName, ref *autoscalingv1beta1.ScalableRef) {
	key, watchable := watchableRef(buffer.Namespace, ref)

	s.mu.Lock()
	defer s.mu.Unlock()

	if prev, ok := s.byBuffer[buffer]; ok {
		if watchable && prev == key {
			return
		}
		s.dropLocked(buffer, prev)
	}
	if !watchable {
		return
	}

	s.byBuffer[buffer] = key
	subs := s.subscribers[key]
	if subs == nil {
		subs = map[types.NamespacedName]struct{}{}
		s.subscribers[key] = subs
	}
	subs[buffer] = struct{}{}
	if len(subs) != 1 {
		return
	}
	s.startWatchLocked(key)
}

// Release drops the watch reference held by a deleted buffer.
func (s *workloadWatchSet) Release(buffer types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.byBuffer[buffer]
	if !ok {
		return
	}
	s.dropLocked(buffer, key)
}

func (s *workloadWatchSet) dropLocked(buffer types.NamespacedName, key workloadKey) {
	delete(s.byBuffer, buffer)
	subs := s.subscribers[key]
	delete(subs, buffer)
	if len(subs) > 0 {
		return
	}
	delete(s.subscribers, key)
	if cancel := s.cancel[key]; cancel != nil {
		cancel()
		delete(s.cancel, key)
	}
}

func (s *workloadWatchSet) run(ctx context.Context, key workloadKey) {
	delay := watchRetryInitial
	for ctx.Err() == nil {
		started := time.Now()
		watcher, err := s.open(ctx, key)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.FromContext(ctx).Error(err, "watching scalableRef workload", key.kind, klog.KRef(key.namespace, key.name))
			if !sleepCtx(ctx, delay) {
				return
			}
			delay = nextWatchDelay(delay)
			continue
		}
		s.consume(ctx, key, watcher)
		watcher.Stop()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= watchStableFor {
			delay = watchRetryInitial
		}
		log.FromContext(ctx).V(1).Info("scalableRef watch closed, restarting", key.kind, klog.KRef(key.namespace, key.name))
		if !sleepCtx(ctx, delay) {
			return
		}
		delay = nextWatchDelay(delay)
	}
}

// consume forwards watch events that can change a buffer's resolved shape or
// replica count. Spec changes (including replicas) bump generation; status-only
// updates do not. The periodic buffer requeue still refreshes a missed event.
func (s *workloadWatchSet) consume(ctx context.Context, key workloadKey, watcher watch.Interface) {
	var generation int64
	seen := false
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-watcher.ResultChan():
			if !ok {
				return
			}
			switch evt.Type {
			case watch.Bookmark:
				continue
			case watch.Added, watch.Modified, watch.Deleted:
			default:
				// Error, or an unrecognized type. Reconnect and try again.
				return
			}
			gen, hasGen := objectGeneration(evt.Object)
			if evt.Type == watch.Modified && seen && hasGen && gen == generation {
				continue
			}
			if hasGen {
				seen = true
				generation = gen
			}
			s.enqueue(ctx, key)
		}
	}
}

func (s *workloadWatchSet) enqueue(ctx context.Context, key workloadKey) {
	s.mu.Lock()
	names := make([]types.NamespacedName, 0, len(s.subscribers[key]))
	for nn := range s.subscribers[key] {
		names = append(names, nn)
	}
	s.mu.Unlock()
	for _, nn := range names {
		if ctx.Err() != nil {
			return
		}
		s.notify(ctx, nn)
	}
}

// watchableRef reports whether ref is a workload kind this controller can resolve.
func watchableRef(namespace string, ref *autoscalingv1beta1.ScalableRef) (workloadKey, bool) {
	if ref == nil || ref.Name == "" || ref.Kind == "" {
		return workloadKey{}, false
	}
	group := ref.APIGroup
	if group == "" {
		group = appsv1.GroupName
	}
	if group != appsv1.GroupName {
		return workloadKey{}, false
	}
	switch ref.Kind {
	case autoscalingv1beta1.KindDeployment, autoscalingv1beta1.KindStatefulSet, autoscalingv1beta1.KindReplicaSet:
		return workloadKey{namespace: namespace, group: group, kind: ref.Kind, name: ref.Name}, true
	default:
		return workloadKey{}, false
	}
}

// workloadListOptions restricts a watch to a single object. metadata.name is a
// supported field selector, so the apiserver never sends the rest of the kind.
func workloadListOptions(key workloadKey) []client.ListOption {
	return []client.ListOption{
		client.InNamespace(key.namespace),
		client.MatchingFields{"metadata.name": key.name},
	}
}

// metadataList is a metadata-only watch target. Generation lives on metadata,
// which is all the watch needs in order to notice a spec change; the full
// object is fetched for that one name when the buffer reconciles.
func (k workloadKey) metadataList() *metav1.PartialObjectMetadataList {
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   k.group,
		Version: appsv1.SchemeGroupVersion.Version,
		Kind:    k.kind + "List",
	})
	return list
}

func objectGeneration(obj any) (int64, bool) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return 0, false
	}
	return accessor.GetGeneration(), true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// cancelledContext is an already-canceled context. Watches parented to it do
// not run until Start installs the leader context.
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func nextWatchDelay(current time.Duration) time.Duration {
	next := current * 2
	if next <= 0 || next > watchRetryMax {
		return watchRetryMax
	}
	return next
}
