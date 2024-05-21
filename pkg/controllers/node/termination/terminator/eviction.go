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

package terminator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	"sigs.k8s.io/karpenter/pkg/operator/controller"

	"sigs.k8s.io/karpenter/pkg/events"
)

const (
	evictionQueueBaseDelay = 100 * time.Millisecond
	evictionQueueMaxDelay  = 10 * time.Second
)

type NodeDrainError struct {
	error
}

func NewNodeDrainError(err error) *NodeDrainError {
	return &NodeDrainError{error: err}
}

func IsNodeDrainError(err error) bool {
	if err == nil {
		return false
	}
	var nodeDrainErr *NodeDrainError
	return errors.As(err, &nodeDrainErr)
}

type QueueKey struct {
	types.NamespacedName
	UID types.UID
}

func NewQueueKey(pod *v1.Pod) QueueKey {
	return QueueKey{
		NamespacedName: client.ObjectKeyFromObject(pod),
		UID:            pod.UID,
	}
}

type Queue struct {
	workqueue.RateLimitingInterface

	mu  sync.Mutex
	set sets.Set[QueueKey]

	kubeClient client.Client
	recorder   events.Recorder
}

func NewQueue(kubeClient client.Client, recorder events.Recorder) *Queue {
	queue := &Queue{
		RateLimitingInterface: workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(evictionQueueBaseDelay, evictionQueueMaxDelay)),
		set:                   sets.New[QueueKey](),
		kubeClient:            kubeClient,
		recorder:              recorder,
	}
	return queue
}

func (q *Queue) Register(_ context.Context, m manager.Manager) error {
	return controller.NewSingletonManagedBy(m).
		Named("eviction-queue").
		Complete(q)
}

// Add adds pods to the Queue
func (q *Queue) Add(pods ...*v1.Pod) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, pod := range pods {
		qk := NewQueueKey(pod)
		if !q.set.Has(qk) {
			q.set.Insert(qk)
			q.RateLimitingInterface.Add(qk)
		}
	}
}

func (q *Queue) Has(pod *v1.Pod) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.set.Has(NewQueueKey(pod))
}

func (q *Queue) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	EvictionQueueDepth.Set(float64(q.RateLimitingInterface.Len()))
	// Check if the queue is empty. client-go recommends not using this function to gate the subsequent
	// get call, but since we're popping items off the queue synchronously, there should be no synchonization
	// issues.
	if q.RateLimitingInterface.Len() == 0 {
		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}
	// Get pod from queue. This waits until queue is non-empty.
	item, shutdown := q.RateLimitingInterface.Get()
	if shutdown {
		return reconcile.Result{}, fmt.Errorf("EvictionQueue is broken and has shutdown")
	}
	qk := item.(QueueKey)
	defer q.RateLimitingInterface.Done(qk)
	// Evict pod
	if q.Evict(ctx, qk) {
		q.RateLimitingInterface.Forget(qk)
		q.mu.Lock()
		q.set.Delete(qk)
		q.mu.Unlock()
		return reconcile.Result{RequeueAfter: controller.Immediately}, nil
	}
	// Requeue pod if eviction failed
	q.RateLimitingInterface.AddRateLimited(qk)
	return reconcile.Result{RequeueAfter: controller.Immediately}, nil
}

// Evict returns true if successful eviction call, and false if not an eviction-related error
func (q *Queue) Evict(ctx context.Context, key QueueKey) bool {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("pod", key.NamespacedName))
	if err := q.kubeClient.SubResource("eviction").Create(ctx,
		&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
		&policyv1.Eviction{
			DeleteOptions: &metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{
					UID: lo.ToPtr(key.UID),
				},
			},
		}); err != nil {
		// status codes for the eviction API are defined here:
		// https://kubernetes.io/docs/concepts/scheduling-eviction/api-eviction/#how-api-initiated-eviction-works
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// 404 - The pod no longer exists
			// https://github.com/kubernetes/kubernetes/blob/ad19beaa83363de89a7772f4d5af393b85ce5e61/pkg/registry/core/pod/storage/eviction.go#L160
			// 409 - The pod exists, but it is not the same pod that we initiated the eviction on
			// https://github.com/kubernetes/kubernetes/blob/ad19beaa83363de89a7772f4d5af393b85ce5e61/pkg/registry/core/pod/storage/eviction.go#L318
			return true
		}
		if apierrors.IsTooManyRequests(err) { // 429 - PDB violation
			q.recorder.Publish(terminatorevents.NodeFailedToDrain(&v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:      key.Name,
				Namespace: key.Namespace,
			}}, fmt.Errorf("evicting pod %s/%s violates a PDB", key.Namespace, key.Name)))
			return false
		}
		log.FromContext(ctx).Error(err, "failed evicting pod")
		return false
	}
	q.recorder.Publish(terminatorevents.EvictPod(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}))
	return true
}

func (q *Queue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.RateLimitingInterface = workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(evictionQueueBaseDelay, evictionQueueMaxDelay))
	q.set = sets.New[QueueKey]()
}
