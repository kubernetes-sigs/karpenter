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

	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
	nodeclaimutil "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

var _ operatorcontroller.FinalizingTypedController[*v1beta1.NodeClaim] = (*Controller)(nil)

// Controller is a NodeClaim Termination controller that triggers deletion of the Node and the
// CloudProvider NodeClaim through its graceful termination mechanism
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController is a constructor for the NodeClaim Controller
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) operatorcontroller.Controller {
	return operatorcontroller.Typed[*v1beta1.NodeClaim](kubeClient, &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	})
}

func (c *Controller) Reconcile(_ context.Context, _ *v1beta1.NodeClaim) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

//nolint:gocyclo
func (c *Controller) Finalize(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", nodeClaim.Status.NodeName, "provider-id", nodeClaim.Status.ProviderID))
	stored := nodeClaim.DeepCopy()
	if !controllerutil.ContainsFinalizer(nodeClaim, v1beta1.TerminationFinalizer) {
		return reconcile.Result{}, nil
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
	if nodeClaim.Status.ProviderID != "" {
		if err = c.cloudProvider.Delete(ctx, nodeClaim); cloudprovider.IgnoreNodeClaimNotFoundError(err) != nil {
			return reconcile.Result{}, fmt.Errorf("terminating cloudprovider instance, %w", err)
		}
	}
	controllerutil.RemoveFinalizer(nodeClaim, v1beta1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		if err = c.kubeClient.Update(ctx, nodeClaim); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(fmt.Errorf("removing termination finalizer, %w", err))
		}
		logging.FromContext(ctx).Infof("deleted nodeclaim")
	}
	return reconcile.Result{}, nil
}

func (*Controller) Name() string {
	return "nodeclaim.termination"
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodeClaim{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Watches(
			&v1.Node{},
			nodeclaimutil.NodeEventHandler(c.kubeClient),
			// Watch for node deletion events
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool { return false },
				UpdateFunc: func(e event.UpdateEvent) bool { return false },
				DeleteFunc: func(e event.DeleteEvent) bool { return true },
			}),
		).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewMaxOfRateLimiter(
				workqueue.NewItemExponentialFailureRateLimiter(time.Second, time.Minute),
				// 10 qps, 100 bucket size
				&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			MaxConcurrentReconciles: 100, // higher concurrency limit since we want fast reaction to termination
		}))
}
