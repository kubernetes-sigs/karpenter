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

package deletioncost

import (
	"context"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	utilscontroller "sigs.k8s.io/karpenter/pkg/utils/controller"
)

const (
	queueBaseDelay = 100 * time.Millisecond
	queueMaxDelay  = 10 * time.Second
	// Concurrency parity with the eviction queue; annotation writes are
	// best-effort so the same linear-scaling shape works.
	minReconciles = 100
	maxReconciles = 5000
)

// QueueKey identifies a pod in the queue. UID is included so a pod replaced
// with the same namespace/name but a different UID between Add and Reconcile
// is treated as a distinct item and cannot inherit stale desired state — this
// mirrors terminator.QueueKey.
type QueueKey struct {
	types.NamespacedName
	UID types.UID
}

func NewQueueKey(pod *corev1.Pod) QueueKey {
	return QueueKey{NamespacedName: client.ObjectKeyFromObject(pod), UID: pod.UID}
}

// queueItem carries the desired annotation state for a single pod. Rank is
// the target value; clear=true means remove the annotation instead of writing
// Rank (Group D).
type queueItem struct {
	rank  int
	clear bool
}

// Queue is a controller-runtime-backed fire-and-forget queue for pod
// deletion-cost annotation writes. It is modeled after the eviction queue
// (pkg/controllers/node/termination/terminator/eviction.go): Add enqueues
// per-pod desired state, Reconcile drains one pod per invocation, and
// controller-runtime provides retry/backoff/logging/metrics plumbing.
type Queue struct {
	sync.Mutex

	source     chan event.TypedGenericEvent[*corev1.Pod]
	items      map[QueueKey]queueItem
	kubeClient client.Client
}

func NewQueue(kubeClient client.Client) *Queue {
	return &Queue{
		source:     make(chan event.TypedGenericEvent[*corev1.Pod], 10000),
		items:      map[QueueKey]queueItem{},
		kubeClient: kubeClient,
	}
}

func (q *Queue) Name() string {
	return "pod.deletioncost.queue"
}

func (q *Queue) Register(ctx context.Context, m manager.Manager) error {
	maxConcurrentReconciles := utilscontroller.LinearScaleReconciles(utilscontroller.CPUCount(ctx), minReconciles, maxReconciles)
	qps, bucketSize := utilscontroller.GetTypedBucketConfigs(100, minReconciles, maxConcurrentReconciles)
	return controllerruntime.NewControllerManagedBy(m).
		Named(q.Name()).
		WatchesRawSource(source.Channel(q.source, handler.TypedFuncs[*corev1.Pod, reconcile.Request]{
			GenericFunc: func(_ context.Context, e event.TypedGenericEvent[*corev1.Pod], queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				queue.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(e.Object)})
			},
		})).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
				workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](queueBaseDelay, queueMaxDelay),
				&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(qps), bucketSize)},
			),
			MaxConcurrentReconciles: maxConcurrentReconciles,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), q))
}

// Add enqueues a desired annotation state for pod. Re-adding overwrites the
// desired state (last-writer-wins) so a rank change between reconciles is
// picked up on the next drain. The channel push only fires on first insertion
// so a burst of Adds for the same pod does not fan out into duplicate work.
func (q *Queue) Add(pod *corev1.Pod, rank int, clear bool) {
	q.Lock()
	defer q.Unlock()

	qk := NewQueueKey(pod)
	_, enqueued := q.items[qk]
	q.items[qk] = queueItem{rank: rank, clear: clear}
	if !enqueued {
		q.source <- event.TypedGenericEvent[*corev1.Pod]{Object: pod}
	}
}

func (q *Queue) Has(pod *corev1.Pod) bool {
	q.Lock()
	defer q.Unlock()
	_, ok := q.items[NewQueueKey(pod)]
	return ok
}

func (q *Queue) complete(qk QueueKey) {
	q.Lock()
	defer q.Unlock()
	delete(q.items, qk)
}

// Reconcile drains one pod's annotation update. Terminal outcomes (success,
// NotFound, Conflict) remove the pod from the queue. Retryable API errors
// return the error so controller-runtime's rate limiter re-enqueues with
// exponential backoff — 429s in particular flow through this path so a
// throttled apiserver naturally slows fan-out across all in-flight pods.
func (q *Queue) Reconcile(ctx context.Context, pod *corev1.Pod) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, q.Name())
	defer metrics.Measure(annotationDurationSeconds, noLabels)()

	qk := NewQueueKey(pod)
	q.Lock()
	item, ok := q.items[qk]
	q.Unlock()
	if !ok {
		// Race: the enqueued pod was replaced (same name/namespace, different
		// UID) before we picked up the reconcile. Matches terminator.Queue.
		return reconcile.Result{}, nil
	}

	if q.matchesDesired(pod, item) {
		q.complete(qk)
		podsUpdatedTotal.Inc(map[string]string{resultLabel: "skipped_unchanged"})
		return reconcile.Result{}, nil
	}

	var err error
	if item.clear {
		err = clearAnnotation(ctx, q.kubeClient, pod)
	} else {
		err = patchAnnotation(ctx, q.kubeClient, pod, strconv.Itoa(item.rank))
	}
	if err == nil {
		podsUpdatedTotal.Inc(map[string]string{resultLabel: "updated"})
		q.complete(qk)
		return reconcile.Result{}, nil
	}
	// NotFound: pod is already gone. Conflict: another writer raced us and
	// won; next reconcile will re-observe. Both are Skipped, not errors.
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("skipping pod annotation update")
		podsUpdatedTotal.Inc(map[string]string{resultLabel: "skipped_unchanged"})
		q.complete(qk)
		return reconcile.Result{}, nil
	}
	podsUpdatedTotal.Inc(map[string]string{resultLabel: "error"})
	return reconcile.Result{}, err
}

func (q *Queue) matchesDesired(pod *corev1.Pod, item queueItem) bool {
	if item.clear {
		_, has := pod.Annotations[corev1.PodDeletionCost]
		return !has
	}
	return pod.Annotations[corev1.PodDeletionCost] == strconv.Itoa(item.rank)
}
