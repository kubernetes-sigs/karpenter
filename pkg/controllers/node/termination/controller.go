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

	"github.com/samber/lo"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/eviction"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

// Controller for the resource
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	reconcilers   []reconciler
}

type reconciler interface {
	Reconcile(context.Context, *corev1.Node, *v1.NodeClaim) (reconcile.Result, error)
}

// NewController constructs a controller instance
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, recorder events.Recorder, evictionQueue *eviction.Queue) *Controller {
	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		reconcilers: []reconciler{
			&DrainReconciler{clk, kubeClient, cloudProvider, recorder, evictionQueue},
			&VolumeDetachmentReconciler{clk, kubeClient, recorder},
			&InstanceTerminationReconciler{clk, kubeClient, cloudProvider},
		},
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.termination").
		For(&corev1.Node{}, builder.WithPredicates(nodeutils.IsManagedPredicateFuncs(c.cloudProvider))).
		Watches(&v1.NodeClaim{}, nodeutils.NodeClaimEventHandler(c.kubeClient, c.cloudProvider)).
		WithOptions(
			controller.Options{
				RateLimiter: workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
					workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](100*time.Millisecond, 10*time.Second),
					// 10 qps, 100 bucket size
					&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
				),
				MaxConcurrentReconciles: 100,
			},
		).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// nolint:gocyclo
func (c *Controller) Reconcile(ctx context.Context, n *corev1.Node) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "node.termination")
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KRef(n.Namespace, n.Name)))

	if n.GetDeletionTimestamp().IsZero() {
		return reconcile.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(n, v1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	if !nodeutils.IsManaged(n, c.cloudProvider) {
		return reconcile.Result{}, nil
	}

	nc, err := nodeutils.NodeClaimForNode(ctx, c.kubeClient, n)
	if err != nil {
		if nodeutils.IsDuplicateNodeClaimError(err) || nodeutils.IsNodeClaimNotFoundError(err) {
			log.FromContext(ctx).Error(err, "failed to terminate node")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("NodeClaim", klog.KRef(nc.Namespace, nc.Name)))
	if nc.DeletionTimestamp.IsZero() {
		if err := c.kubeClient.Delete(ctx, nc); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}

	if err := c.prepareNode(ctx, n); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, fmt.Errorf("tainting node with %s, %w", pretty.Taint(v1.DisruptedNoScheduleTaint), err)
	}

	for _, r := range c.reconcilers {
		res, err := r.Reconcile(ctx, n, nc)
		if res.Requeue || res.RequeueAfter != 0 || err != nil {
			return res, err
		}
	}
	return reconcile.Result{}, nil
}

// prepareNode ensures that the node is ready to begin the drain / termination process. This includes ensuring that it
// is tainted appropriately and annotated to be ignored by external load balancers.
func (c *Controller) prepareNode(ctx context.Context, n *corev1.Node) error {
	stored := n.DeepCopy()

	// Add the karpenter.sh/disrupted:NoSchedule taint to ensure no additional pods schedule to the Node during the
	// drain process.
	if !lo.ContainsBy(n.Spec.Taints, func(t corev1.Taint) bool {
		return t.MatchTaint(&v1.DisruptedNoScheduleTaint)
	}) {
		n.Spec.Taints = append(n.Spec.Taints, v1.DisruptedNoScheduleTaint)
	}
	// Adding this label to the node ensures that the node is removed from the load-balancer target group while it is
	// draining and before it is terminated. This prevents 500s coming prior to health check when the load balancer
	// controller hasn't yet determined that the node and underlying connections are gone.
	// https://github.com/aws/aws-node-termination-handler/issues/316
	// https://github.com/aws/karpenter/pull/2518
	n.Labels = lo.Assign(n.Labels, map[string]string{
		corev1.LabelNodeExcludeBalancers: "karpenter",
	})

	if equality.Semantic.DeepEqual(n, stored) {
		return nil
	}
	// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch can cause races due
	// to the fact that it fully replaces the list on a change. Here, we are updating the taint list.
	return c.kubeClient.Patch(ctx, n, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{}))
}

// removeFinalizer removes Karpenter's termination finalizer from the given Node, updating the termination metrics in the process.
func removeFinalizer(ctx context.Context, kubeClient client.Client, n *corev1.Node) error {
	stored := n.DeepCopy()
	controllerutil.RemoveFinalizer(n, v1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, n) {
		// We use client.StrategicMergeFrom here since the node object supports it and
		// a strategic merge patch represents the finalizer list as a keyed "set" so removing
		// an item from the list doesn't replace the full list
		// https://github.com/kubernetes/kubernetes/issues/111643#issuecomment-2016489732
		if err := kubeClient.Patch(ctx, n, client.StrategicMergeFrom(stored)); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("removing finalizer, %w", err)
		}
		metrics.NodesTerminatedTotal.Inc(map[string]string{
			metrics.NodePoolLabel: n.Labels[v1.NodePoolLabelKey],
		})
		// We use stored.DeletionTimestamp since the api-server may give back a node after the patch without a deletionTimestamp
		DurationSeconds.Observe(time.Since(stored.DeletionTimestamp.Time).Seconds(), map[string]string{
			metrics.NodePoolLabel: n.Labels[v1.NodePoolLabelKey],
		})
		NodeLifetimeDurationSeconds.Observe(time.Since(n.CreationTimestamp.Time).Seconds(), map[string]string{
			metrics.NodePoolLabel: n.Labels[v1.NodePoolLabelKey],
		})
		log.FromContext(ctx).Info("deleted node")
	}
	return nil
}
