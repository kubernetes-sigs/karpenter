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

package termination

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/utils/termination"

	"sigs.k8s.io/karpenter/pkg/metrics"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	"sigs.k8s.io/karpenter/pkg/events"
	nodeclaimutil "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// Controller is a NodeClaim Termination controller that triggers deletion of the Node and the
// CloudProvider NodeClaim through its graceful termination mechanism
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	recorder      events.Recorder
}

// NewController is a constructor for the NodeClaim Controller
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, recorder events.Recorder) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		recorder:      recorder,
	}
}

func (c *Controller) Reconcile(ctx context.Context, n *v1.NodeClaim) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodeclaim.termination")

	if !n.GetDeletionTimestamp().IsZero() {
		return c.finalize(ctx, n)
	}
	return reconcile.Result{}, nil
}

//nolint:gocyclo
func (c *Controller) finalize(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KRef("", nodeClaim.Status.NodeName), "provider-id", nodeClaim.Status.ProviderID))
	stored := nodeClaim.DeepCopy()
	if !controllerutil.ContainsFinalizer(nodeClaim, v1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	if err := c.ensureTerminationGracePeriodTerminationTimeAnnotation(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, fmt.Errorf("adding nodeclaim terminationGracePeriod annotation, %w", err)
	}
	nodes, err := nodeclaimutil.AllNodesForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		return reconcile.Result{}, err
	}
	for _, node := range nodes {
		// If we still get the Node, but it's already marked as terminating, we don't need to call Delete again
		if node.DeletionTimestamp.IsZero() {
			// We delete nodes to trigger the node finalization and deletion flow
			if err = c.kubeClient.Delete(ctx, node); client.IgnoreNotFound(err) != nil {
				return reconcile.Result{}, err
			}
		}
	}
	// We wait until all the nodes associated with this nodeClaim have completed their deletion before triggering the finalization of the nodeClaim
	if len(nodes) > 0 {
		return reconcile.Result{}, nil
	}
	var isInstanceTerminated bool
	// We can expect ProviderID to be empty when there is a failure while launching the nodeClaim
	if nodeClaim.Status.ProviderID != "" {
		isInstanceTerminated, err = termination.EnsureTerminated(ctx, c.kubeClient, nodeClaim, c.cloudProvider)
		if err != nil {
			// 404 = the nodeClaim no longer exists
			if errors.IsNotFound(err) {
				return reconcile.Result{}, nil
			}
			// 409 - The nodeClaim exists, but its status has already been modified
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, fmt.Errorf("ensuring instance termination, %w", err)
		}
		if !isInstanceTerminated {
			return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
		}
		InstanceTerminationDurationSeconds.With(map[string]string{
			metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
		}).Observe(time.Since(nodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).LastTransitionTime.Time).Seconds())
	}
	controllerutil.RemoveFinalizer(nodeClaim, v1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		// We call Update() here rather than Patch() because patching a list with a JSON merge patch
		// can cause races due to the fact that it fully replaces the list on a change
		// https://github.com/kubernetes/kubernetes/issues/111643#issuecomment-2016489732
		if err = c.kubeClient.Update(ctx, nodeClaim); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(fmt.Errorf("removing termination finalizer, %w", err))
		}
		log.FromContext(ctx).Info("deleted nodeclaim")
		NodeClaimTerminationDurationSeconds.With(map[string]string{
			metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
		}).Observe(time.Since(stored.DeletionTimestamp.Time).Seconds())
		metrics.NodeClaimsTerminatedTotal.With(prometheus.Labels{
			metrics.NodePoolLabel:     nodeClaim.Labels[v1.NodePoolLabelKey],
			metrics.CapacityTypeLabel: nodeClaim.Labels[v1.CapacityTypeLabelKey],
		}).Inc()
	}
	return reconcile.Result{}, nil
}

func (c *Controller) ensureTerminationGracePeriodTerminationTimeAnnotation(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	// if the expiration annotation is already set, we don't need to do anything
	if _, exists := nodeClaim.ObjectMeta.Annotations[v1.NodeClaimTerminationTimestampAnnotationKey]; exists {
		return nil
	}

	// In Kubernetes, every object has a terminationGracePeriodSeconds, defaulted to and un-changeable from 0. There is an additional TerminationGracePeriodSeconds in the PodSpec which can be configured.
	// We use the kubernetes object TerminationGracePeriod to infer that the DeletionTimestamp is always equal to the time the NodeClaim is deleted.
	// This should not be confused with the NodeClaim.spec.terminationGracePeriod field introduced in Karpenter Custom Resources.
	if nodeClaim.Spec.TerminationGracePeriod != nil && !nodeClaim.ObjectMeta.DeletionTimestamp.IsZero() {
		terminationTimeString := nodeClaim.DeletionTimestamp.Time.Add(nodeClaim.Spec.TerminationGracePeriod.Duration).Format(time.RFC3339)
		return c.annotateTerminationGracePeriodTerminationTime(ctx, nodeClaim, terminationTimeString)
	}

	return nil
}

func (c *Controller) annotateTerminationGracePeriodTerminationTime(ctx context.Context, nodeClaim *v1.NodeClaim, terminationTime string) error {
	stored := nodeClaim.DeepCopy()
	nodeClaim.ObjectMeta.Annotations = lo.Assign(nodeClaim.ObjectMeta.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: terminationTime})

	if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("patching nodeclaim, %w", err))
	}
	log.FromContext(ctx).WithValues(v1.NodeClaimTerminationTimestampAnnotationKey, terminationTime).Info("annotated nodeclaim")
	c.recorder.Publish(terminatorevents.NodeClaimTerminationGracePeriodExpiring(nodeClaim, terminationTime))

	return nil
}

func (*Controller) Name() string {
	return ""
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclaim.termination").
		For(&v1.NodeClaim{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Watches(
			&corev1.Node{},
			nodeclaimutil.NodeEventHandler(c.kubeClient),
			// Watch for node deletion events
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool { return false },
				UpdateFunc: func(e event.UpdateEvent) bool { return false },
				DeleteFunc: func(e event.DeleteEvent) bool { return true },
			}),
		).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
				workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Second, time.Minute),
				// 10 qps, 100 bucket size
				&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			MaxConcurrentReconciles: 100, // higher concurrency limit since we want fast reaction to termination
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
