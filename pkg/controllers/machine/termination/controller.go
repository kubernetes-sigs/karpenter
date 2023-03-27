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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

var _ corecontroller.FinalizingTypedController[*v1alpha5.Machine] = (*Controller)(nil)

// Controller is a Machine Termination controller that triggers deletion of the Node and the
// CloudProvider Machine through its graceful termination mechanism
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController is a constructor for the Machine Controller
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	})
}

func (*Controller) Name() string {
	return "machine_termination"
}

func (c *Controller) Reconcile(_ context.Context, _ *v1alpha5.Machine) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (c *Controller) Finalize(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	stored := machine.DeepCopy()
	if !controllerutil.ContainsFinalizer(machine, v1alpha5.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	nodes, err := machineutil.AllNodesForMachine(ctx, c.kubeClient, machine)
	if err != nil {
		return reconcile.Result{}, err
	}
	for _, node := range nodes {
		// We delete nodes to trigger the node finalization and deletion flow
		if err = c.kubeClient.Delete(ctx, node); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, err
		}
	}
	// We wait until all the nodes associated with this machine have completed their deletion before triggering the finalization of the machine
	if len(nodes) > 0 {
		return reconcile.Result{}, nil
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provisioner", machine.Labels[v1alpha5.ProvisionerNameLabelKey], "provider-id", machine.Status.ProviderID))
	if machine.Status.ProviderID != "" {
		if err := c.cloudProvider.Delete(ctx, machine); cloudprovider.IgnoreMachineNotFoundError(err) != nil {
			return reconcile.Result{}, fmt.Errorf("terminating cloudprovider instance, %w", err)
		}
	}
	controllerutil.RemoveFinalizer(machine, v1alpha5.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, machine) {
		if err := c.kubeClient.Patch(ctx, machine, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(fmt.Errorf("removing machine termination finalizer, %w", err))
		}
		logging.FromContext(ctx).Infof("deleted machine")
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Machine{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			machineutil.NodeEventHandler(ctx, c.kubeClient),
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
