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
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/node/termination/terminator"
	terminatorevents "github.com/aws/karpenter-core/pkg/controllers/node/termination/terminator/events"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

var _ corecontroller.FinalizingTypedController[*v1.Node] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	terminator    *terminator.Terminator
	recorder      events.Recorder
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, terminator *terminator.Terminator, recorder events.Recorder) corecontroller.Controller {
	return corecontroller.Typed[*v1.Node](kubeClient, &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		terminator:    terminator,
		recorder:      recorder,
	})
}

func (c *Controller) Name() string {
	return "node.termination"
}

func (c *Controller) Reconcile(_ context.Context, _ *v1.Node) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

//nolint:gocyclo
func (c *Controller) Finalize(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
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
		// If the underlying machine no longer exists.
		if _, err := c.cloudProvider.Get(ctx, node.Spec.ProviderID); err != nil {
			if cloudprovider.IsNodeClaimNotFoundError(err) {
				return reconcile.Result{}, c.removeFinalizer(ctx, node)
			}
			return reconcile.Result{}, fmt.Errorf("getting machine, %w", err)
		}
		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}
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
		if err := c.kubeClient.Delete(ctx, &nodeClaimList.Items[i]); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	return nil
}

func (c *Controller) removeFinalizer(ctx context.Context, n *v1.Node) error {
	stored := n.DeepCopy()
	controllerutil.RemoveFinalizer(n, v1beta1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, n) {
		if err := c.kubeClient.Patch(ctx, n, client.MergeFrom(stored)); err != nil {
			return client.IgnoreNotFound(fmt.Errorf("patching node, %w", err))
		}
		metrics.NodesTerminatedCounter.With(prometheus.Labels{
			metrics.NodePoolLabel: n.Labels[v1beta1.NodePoolLabelKey],
		}).Inc()
		// We use stored.DeletionTimestamp since the api-server may give back a node after the patch without a deletionTimestamp
		TerminationSummary.With(prometheus.Labels{
			metrics.NodePoolLabel: n.Labels[v1beta1.NodePoolLabelKey],
		}).Observe(time.Since(stored.DeletionTimestamp.Time).Seconds())
		logging.FromContext(ctx).Infof("deleted node")
	}
	return nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
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
		))
}
