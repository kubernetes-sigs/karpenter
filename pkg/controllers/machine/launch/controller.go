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

package launch

import (
	"context"
	"time"

	"github.com/patrickmn/go-cache"
	"go.uber.org/multierr"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/result"
)

type machineReconciler interface {
	Reconcile(context.Context, *v1alpha5.Machine) (reconcile.Result, error)
}

var _ corecontroller.TypedController[*v1alpha5.Machine] = (*Controller)(nil)

// Controller is a Launch controller which has a high concurrency limit and works to launch and propagate
// launch data into the Machines as quickly as possible. This launch controller also controls timeouts if the
// launch doesn't execute and finish within a defined launchTTL
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider

	launch  *Launch
	timeout *Timeout
}

func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,

		launch:  &Launch{kubeClient: kubeClient, cloudProvider: cloudProvider, cache: cache.New(time.Minute, time.Second*10)},
		timeout: &Timeout{clock: clk, kubeClient: kubeClient},
	})
}

func (*Controller) Name() string {
	return "machine_launch"
}

func (c *Controller) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if !machine.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	if machine.StatusConditions().GetCondition(v1alpha5.MachineCreated).IsTrue() {
		return reconcile.Result{}, nil
	}

	// Add the finalizer immediately since we shouldn't launch if we don't yet have the finalizer.
	// Otherwise, we could leak resources
	stored := machine.DeepCopy()
	controllerutil.AddFinalizer(machine, v1alpha5.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(machine, stored) {
		if err := c.kubeClient.Patch(ctx, machine, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}

	stored = machine.DeepCopy()
	var results []reconcile.Result
	var errs error
	for _, reconciler := range []machineReconciler{
		c.launch,
		c.timeout, // we check timeout last, since we don't want to delete the machine, and then still launch
	} {
		res, err := reconciler.Reconcile(ctx, machine)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}
	if !equality.Semantic.DeepEqual(stored, machine) {
		statusCopy := machine.DeepCopy()
		if err := c.kubeClient.Patch(ctx, machine, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(multierr.Append(errs, err))
		}
		if err := c.kubeClient.Status().Patch(ctx, statusCopy, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(multierr.Append(errs, err))
		}
	}
	return result.Min(results...), errs
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Machine{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(_ event.CreateEvent) bool { return true },
			UpdateFunc: func(_ event.UpdateEvent) bool { return false },
			DeleteFunc: func(_ event.DeleteEvent) bool { return false },
		}).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewMaxOfRateLimiter(
				workqueue.NewItemExponentialFailureRateLimiter(time.Second, time.Minute),
				// 10 qps, 100 bucket size
				&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			// Higher concurrency limit since we want faster reaction to launch
			// This high of a concurrency limit should be localized to this launch controller since it is trying to
			// launch machines as quickly and as efficiently as possible
			MaxConcurrentReconciles: 1000,
		}))
}

// removeFinalizerBestEffort attempts to Patch out the Finalizer to be more efficient and save time
// This isn't necessary but saves us from running through the standard termination loop when we know that the
// cloudprovider machine doesn't exist (since we haven't launched) so there's no use going through the finalization loop
// It returns a boolean representing if it was able to successfully remove the finalizer
func removeFinalizerBestEffort(ctx context.Context, c client.Client, machine *v1alpha5.Machine) bool {
	stored := machine.DeepCopy()
	controllerutil.RemoveFinalizer(machine, v1alpha5.TerminationFinalizer)
	err := c.Patch(ctx, machine, client.MergeFrom(stored))
	return err == nil
}
