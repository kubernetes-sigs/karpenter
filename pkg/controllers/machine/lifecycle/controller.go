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

package lifecycle

import (
	"context"
	"time"

	"github.com/patrickmn/go-cache"
	"go.uber.org/multierr"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	"github.com/aws/karpenter-core/pkg/utils/result"
)

type machineReconciler interface {
	Reconcile(context.Context, *v1alpha5.Machine) (reconcile.Result, error)
}

var _ corecontroller.TypedController[*v1alpha5.Machine] = (*Controller)(nil)

// Controller is a Machine Lifecycle controller that manages the lifecycle of the machine up until its termination
// The controller is responsible for ensuring that new Machines get launched, that they have properly registered with
// the cluster as nodes and that they are properly initialized, ensuring that Machines that do not have matching nodes
// after some liveness TTL are removed
type Controller struct {
	kubeClient client.Client

	launch         *Launch
	registration   *Registration
	initialization *Initialization
	liveness       *Liveness
}

func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &Controller{
		kubeClient: kubeClient,

		launch:         &Launch{kubeClient: kubeClient, cloudProvider: cloudProvider, cache: cache.New(time.Minute, time.Second*10)},
		registration:   &Registration{kubeClient: kubeClient},
		initialization: &Initialization{kubeClient: kubeClient},
		liveness:       &Liveness{clock: clk, kubeClient: kubeClient},
	})
}

func (*Controller) Name() string {
	return "machine_lifecycle"
}

func (c *Controller) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provisioner", machine.Labels[v1alpha5.ProvisionerNameLabelKey]))
	if !machine.DeletionTimestamp.IsZero() {
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
		c.registration,
		c.initialization,
		c.liveness,
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
		// We sleep here after a patch operation since we want to ensure that we are able to read our own writes
		// so that we avoid duplicating metrics and log lines due to quick re-queues from our node watcher
		// USE CAUTION when determining whether to increase this timeout or remove this line
		time.Sleep(time.Second)
	}
	return result.Min(results...), errs
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Machine{}, builder.WithPredicates(
			predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool { return true },
				UpdateFunc: func(e event.UpdateEvent) bool { return false },
				DeleteFunc: func(e event.DeleteEvent) bool { return false },
			},
		)).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			machineutil.NodeEventHandler(ctx, c.kubeClient),
		).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewMaxOfRateLimiter(
				workqueue.NewItemExponentialFailureRateLimiter(time.Second, time.Minute),
				// 10 qps, 100 bucket size
				&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			MaxConcurrentReconciles: 1000, // higher concurrency limit since we want fast reaction to node syncing and launch
		}))
}
