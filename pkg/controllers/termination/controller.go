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
	"time"

	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/machine/terminator"
	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

var _ corecontroller.FinalizingTypedController[*v1.Node] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	terminator *terminator.Terminator
	kubeClient client.Client
	recorder   events.Recorder
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, terminator *terminator.Terminator, recorder events.Recorder) corecontroller.Controller {
	return corecontroller.Typed[*v1.Node](kubeClient, &Controller{
		kubeClient: kubeClient,
		terminator: terminator,
		recorder:   recorder,
	})
}

func (c *Controller) Name() string {
	return "termination"
}

func (c *Controller) Reconcile(_ context.Context, _ *v1.Node) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (c *Controller) Finalize(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(node, v1alpha5.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	machineList := &v1alpha5.MachineList{}
	if err := c.kubeClient.List(ctx, machineList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
		return reconcile.Result{}, err
	}
	if len(machineList.Items) == 0 {
		stored := node.DeepCopy()
		controllerutil.RemoveFinalizer(node, v1alpha5.TerminationFinalizer)
		if !equality.Semantic.DeepEqual(stored, node) {
			if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
				return reconcile.Result{}, client.IgnoreNotFound(err)
			}
		}
		return reconcile.Result{}, nil
	}
	for i := range machineList.Items {
		if err := c.kubeClient.Delete(ctx, &machineList.Items[i]); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
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
				MaxConcurrentReconciles: 10,
			},
		))
}
