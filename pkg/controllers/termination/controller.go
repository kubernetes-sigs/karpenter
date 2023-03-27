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

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/machine/terminator"
	terminatorevents "github.com/aws/karpenter-core/pkg/controllers/machine/terminator/events"
	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	"github.com/samber/lo"
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
	return "termination"
}

func (c *Controller) Reconcile(_ context.Context, _ *v1.Node) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

//nolint:gocyclo
func (c *Controller) Finalize(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(node, v1alpha5.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	if err := c.terminator.Cordon(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("cordoning node, %w", err)
	}
	if err := c.terminator.Drain(ctx, node); err != nil {
		if !terminator.IsNodeDrainError(err) {
			return reconcile.Result{}, fmt.Errorf("draining node, %w", err)
		}
		c.recorder.Publish(terminatorevents.NodeFailedToDrain(node, err))
		// If pods cannot be drained, check if the node lifecycle controller has tainted the node as unreachable.
		if _, found := lo.Find(node.Spec.Taints, func(taint v1.Taint) bool {
			return taint.MatchTaint(&v1.Taint{Key: v1.TaintNodeUnreachable, Effect: v1.TaintEffectNoExecute})
		}); !found {
			return reconcile.Result{Requeue: true}, nil
		}
		// If the node is unreachable, complete node deletion if the underlying machine no longer exists.
		// If the node exists, requeue after a second to not hammer the API.
		if _, err := c.cloudProvider.Get(ctx, node.Spec.ProviderID); !cloudprovider.IsMachineNotFoundError(err) {
			return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
		}
	}
	if err := c.cloudProvider.Delete(ctx, machineutil.NewFromNode(node)); cloudprovider.IgnoreMachineNotFoundError(err) != nil {
		return reconcile.Result{}, fmt.Errorf("terminating cloudprovider instance, %w", err)
	}
	stored := node.DeepCopy()
	controllerutil.RemoveFinalizer(node, v1alpha5.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, node) {
		if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		logging.FromContext(ctx).Infof("deleted node")
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) corecontroller.Builder {
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
				MaxConcurrentReconciles: 10,
			},
		))
}
