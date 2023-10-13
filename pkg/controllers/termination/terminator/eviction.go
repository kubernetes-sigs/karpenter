/*
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
	"time"

	set "github.com/deckarep/golang-set"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/operator/controller"

	terminatorevents "github.com/aws/karpenter-core/pkg/controllers/termination/terminator/events"
	"github.com/aws/karpenter-core/pkg/events"
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

type Queue struct {
	workqueue.RateLimitingInterface
	set.Set

	coreV1Client corev1.CoreV1Interface
	recorder     events.Recorder
}

func NewQueue(coreV1Client corev1.CoreV1Interface, recorder events.Recorder) *Queue {
	queue := &Queue{
		RateLimitingInterface: workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(evictionQueueBaseDelay, evictionQueueMaxDelay)),
		Set:                   set.NewSet(),
		coreV1Client:          coreV1Client,
		recorder:              recorder,
	}
	return queue
}

func (q *Queue) Name() string {
	return "eviction-queue"
}

func (q *Queue) Builder(_ context.Context, m manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(m)
}

// Add adds pods to the Queue
func (q *Queue) Add(pods ...*v1.Pod) {
	for _, pod := range pods {
		if nn := client.ObjectKeyFromObject(pod); !q.Set.Contains(nn) {
			q.Set.Add(nn)
			q.RateLimitingInterface.Add(nn)
		}
	}
}

func (q *Queue) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// Check if the queue is empty. client-go recommends not using this function to gate the subsequent
	// get call, but since we're popping items off the queue synchronously, there should be no synchonization
	// issues.
	if q.Len() == 0 {
		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}
	// Get pod from queue. This waits until queue is non-empty.
	item, shutdown := q.RateLimitingInterface.Get()
	if shutdown {
		q.ShutDown()
		return reconcile.Result{}, fmt.Errorf("EvictionQueue is broken and has shutdown")
	}
	nn := item.(types.NamespacedName)
	defer q.RateLimitingInterface.Done(nn)
	// Evict pod
	if q.Evict(ctx, nn) {
		q.RateLimitingInterface.Forget(nn)
		q.Set.Remove(nn)
		return reconcile.Result{RequeueAfter: controller.Immediately}, nil
	}
	// Requeue pod if eviction failed
	q.RateLimitingInterface.AddRateLimited(nn)
	return reconcile.Result{RequeueAfter: controller.Immediately}, nil
}

// Evict returns true if successful eviction call, and false if not an eviction-related error
func (q *Queue) Evict(ctx context.Context, nn types.NamespacedName) bool {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("pod", nn))
	if err := q.coreV1Client.Pods(nn.Namespace).EvictV1(ctx, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
	}); err != nil {
		// status codes for the eviction API are defined here:
		// https://kubernetes.io/docs/concepts/scheduling-eviction/api-eviction/#how-api-initiated-eviction-works
		if apierrors.IsNotFound(err) { // 404
			return true
		}
		if apierrors.IsTooManyRequests(err) { // 429 - PDB violation
			q.recorder.Publish(terminatorevents.NodeFailedToDrain(&v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:      nn.Name,
				Namespace: nn.Namespace,
			}}, fmt.Errorf("evicting pod %s/%s violates a PDB", nn.Namespace, nn.Name)))
			return false
		}
		logging.FromContext(ctx).Errorf("evicting pod, %s", err)
		return false
	}
	q.recorder.Publish(terminatorevents.EvictPod(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace}}))
	return true
}

func (q *Queue) Reset() {
	q.RateLimitingInterface = workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(evictionQueueBaseDelay, evictionQueueMaxDelay))
	q.Set = set.NewSet()
}
