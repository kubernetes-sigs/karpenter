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

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
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

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
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
	sync.Mutex

	source chan event.TypedGenericEvent[*corev1.Pod]
	set    sets.Set[client.ObjectKey]

	kubeClient client.Client
	recorder   events.Recorder
}

func NewQueue(kubeClient client.Client, recorder events.Recorder) *Queue {
	return &Queue{
		source:     make(chan event.TypedGenericEvent[*corev1.Pod], 10000),
		set:        sets.New[client.ObjectKey](),
		kubeClient: kubeClient,
		recorder:   recorder,
	}
}

func (q *Queue) Name() string {
	return "eviction-queue"
}

func (q *Queue) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(q.Name()).
		WatchesRawSource(source.Channel(q.source, handler.TypedFuncs[*corev1.Pod, reconcile.Request]{
			GenericFunc: func(_ context.Context, e event.TypedGenericEvent[*corev1.Pod], queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				queue.Add(reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(e.Object),
				})
			},
		})).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 100,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), q))
}

// Add adds pods to the Queue
func (q *Queue) Add(pods ...*corev1.Pod) {
	q.Lock()
	defer q.Unlock()

	for _, pod := range pods {
		qk := client.ObjectKeyFromObject(pod)
		if !q.set.Has(qk) {
			q.set.Insert(qk)
			q.source <- event.TypedGenericEvent[*corev1.Pod]{Object: pod}
		}
	}
}

func (q *Queue) Has(pod *corev1.Pod) bool {
	q.Lock()
	defer q.Unlock()

	return q.set.Has(client.ObjectKeyFromObject(pod))
}

func (q *Queue) Reconcile(ctx context.Context, pod *corev1.Pod) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, q.Name())

	// Evict the pod
	err := q.Evict(ctx, pod)
	if err != nil {
		return reconcile.Result{}, err
	}

	q.Lock()
	q.set.Delete(client.ObjectKeyFromObject(pod))
	q.Unlock()
	return reconcile.Result{}, nil
}

// Evict returns nil if successful eviction call, and an error if there was an eviction-related error
func (q *Queue) Evict(ctx context.Context, pod *corev1.Pod) error {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Pod", klog.KRef(pod.Namespace, pod.Name)))
	if err := q.kubeClient.SubResource("eviction").Create(ctx,
		pod,
		&policyv1.Eviction{
			DeleteOptions: &metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{
					UID: lo.ToPtr(pod.UID),
				},
			},
		}); err != nil {
		var apiStatus apierrors.APIStatus
		if errors.As(err, &apiStatus) {
			code := apiStatus.Status().Code
			NodesEvictionRequestsTotal.Inc(map[string]string{CodeLabel: fmt.Sprint(code)})
		}
		// status codes for the eviction API are defined here:
		// https://kubernetes.io/docs/concepts/scheduling-eviction/api-eviction/#how-api-initiated-eviction-works
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// 404 - The pod no longer exists
			// https://github.com/kubernetes/kubernetes/blob/ad19beaa83363de89a7772f4d5af393b85ce5e61/pkg/registry/core/pod/storage/eviction.go#L160
			// 409 - The pod exists, but it is not the same pod that we initiated the eviction on
			// https://github.com/kubernetes/kubernetes/blob/ad19beaa83363de89a7772f4d5af393b85ce5e61/pkg/registry/core/pod/storage/eviction.go#L318
			return nil
		}
		if apierrors.IsTooManyRequests(err) { // 429 - PDB violation
			q.recorder.Publish(terminatorevents.NodeFailedToDrain(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			}}, serrors.Wrap(fmt.Errorf("evicting pod violates a PDB"), "Pod", klog.KRef(pod.Namespace, pod.Name))))
			return err
		}
		log.FromContext(ctx).Error(err, "failed evicting pod")
		return err
	}
	NodesEvictionRequestsTotal.Inc(map[string]string{CodeLabel: "200"})
	reason := evictionReason(ctx, pod, q.kubeClient)
	q.recorder.Publish(terminatorevents.EvictPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}, reason))
	PodsDrainedTotal.Inc(map[string]string{ReasonLabel: reason})
	return nil
}

func evictionReason(ctx context.Context, pod *corev1.Pod, kubeClient client.Client) string {
	node, err := podutils.NodeForPod(ctx, kubeClient, pod)
	if err != nil {
		log.FromContext(ctx).V(1).Error(err, "pod has no node, failed looking up pod eviction reason")
		return ""
	}
	nodeClaim, err := nodeutils.NodeClaimForNode(ctx, kubeClient, node)
	if err != nil {
		log.FromContext(ctx).V(1).Error(err, "node has no nodeclaim, failed looking up pod eviction reason")
		return ""
	}
	if cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeDisruptionReason); cond.IsTrue() {
		return cond.Reason
	}
	return "Forceful Termination"
}
