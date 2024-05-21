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
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutil "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// Controller for the resource
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	terminator    *terminator.Terminator
	recorder      events.Recorder
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, terminator *terminator.Terminator, recorder events.Recorder) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		terminator:    terminator,
		recorder:      recorder,
	}
}

func (c *Controller) Reconcile(ctx context.Context, n *v1.Node) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithName("node.termination").WithValues("node", n.Name))
	ctx = injection.WithControllerName(ctx, "node.termination")

	if !n.GetDeletionTimestamp().IsZero() {
		return c.finalize(ctx, n)
	}
	return reconcile.Result{}, nil
}

//nolint:gocyclo
func (c *Controller) finalize(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(node, v1beta1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	if err := c.deleteAllNodeClaims(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("deleting nodeclaims, %w", err)
	}
	if err := c.terminator.Taint(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("tainting node, %w", err)
	}
	if err := c.terminator.Drain(ctx, node); err != nil {
		if !terminator.IsNodeDrainError(err) {
			return reconcile.Result{}, fmt.Errorf("draining node, %w", err)
		}
		c.recorder.Publish(terminatorevents.NodeFailedToDrain(node, err))
		// If the underlying NodeClaim no longer exists, we want to delete to avoid trying to gracefully draining
		// on nodes that are no longer alive. We do a check on the Ready condition of the node since, even
		// though the CloudProvider says the instance is not around, we know that the kubelet process is still running
		// if the Node Ready condition is true
		// Similar logic to: https://github.com/kubernetes/kubernetes/blob/3a75a8c8d9e6a1ebd98d8572132e675d4980f184/staging/src/k8s.io/cloud-provider/controllers/nodelifecycle/node_lifecycle_controller.go#L144
		if nodeutils.GetCondition(node, v1.NodeReady).Status != v1.ConditionTrue {
			if _, err := c.cloudProvider.Get(ctx, node.Spec.ProviderID); err != nil {
				if cloudprovider.IsNodeClaimNotFoundError(err) {
					return reconcile.Result{}, c.removeFinalizer(ctx, node)
				}
				return reconcile.Result{}, fmt.Errorf("getting nodeclaim, %w", err)
			}
		}
		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}
	// Be careful when removing this delete call in the Node termination flow
	// This delete call is needed so that we ensure that we don't remove the node from the cluster
	// until the full instance shutdown has taken place
	if err := c.cloudProvider.Delete(ctx, nodeclaimutil.NewFromNode(node)); cloudprovider.IgnoreNodeClaimNotFoundError(err) != nil {
		return reconcile.Result{}, fmt.Errorf("terminating cloudprovider instance, %w", err)
	}
	if err := c.removeFinalizer(ctx, node); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (c *Controller) deleteAllNodeClaims(ctx context.Context, node *v1.Node) error {
	nodeClaimList := &v1beta1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaimList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
		return err
	}
	for i := range nodeClaimList.Items {
		// If we still get the NodeClaim, but it's already marked as terminating, we don't need to call Delete again
		if nodeClaimList.Items[i].DeletionTimestamp.IsZero() {
			if err := c.kubeClient.Delete(ctx, &nodeClaimList.Items[i]); err != nil {
				return client.IgnoreNotFound(err)
			}
		}
	}
	return nil
}

func (c *Controller) removeFinalizer(ctx context.Context, n *v1.Node) error {
	stored := n.DeepCopy()
	controllerutil.RemoveFinalizer(n, v1beta1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, n) {
		if err := c.kubeClient.Patch(ctx, n, client.StrategicMergeFrom(stored)); err != nil {
			return client.IgnoreNotFound(fmt.Errorf("patching node, %w", err))
		}
		metrics.NodesTerminatedCounter.With(prometheus.Labels{
			metrics.NodePoolLabel: n.Labels[v1beta1.NodePoolLabelKey],
		}).Inc()
		// We use stored.DeletionTimestamp since the api-server may give back a node after the patch without a deletionTimestamp
		TerminationSummary.With(prometheus.Labels{
			metrics.NodePoolLabel: n.Labels[v1beta1.NodePoolLabelKey],
		}).Observe(time.Since(stored.DeletionTimestamp.Time).Seconds())
		log.FromContext(ctx).Info("deleted node")
	}
	return nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.termination").
		For(&v1.Node{}).
		WithOptions(
			controller.Options{
				RateLimiter: workqueue.NewMaxOfRateLimiter(
					workqueue.NewItemExponentialFailureRateLimiter(100*time.Millisecond, 10*time.Second),
					// 10 qps, 100 bucket size
					&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
				),
				MaxConcurrentReconciles: 100,
			},
		).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
