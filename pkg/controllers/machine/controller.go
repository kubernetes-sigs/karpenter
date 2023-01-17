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

package machine

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/result"
)

// Controller is a Machine Controller
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider

	launch         *Launch
	registration   *Registration
	initialization *Initialization
	liveness       *Liveness
}

// NewController is a constructor for the Machine Controller
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,

		launch:         &Launch{kubeClient: kubeClient, cloudProvider: cloudProvider},
		registration:   &Registration{kubeClient: kubeClient},
		initialization: &Initialization{kubeClient: kubeClient},
		liveness:       &Liveness{kubeClient: kubeClient},
	})
}

func (*Controller) Name() string {
	return "machine"
}

func (c *Controller) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
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
	for _, reconciler := range []interface {
		Reconcile(context.Context, *v1alpha5.Machine) (reconcile.Result, error)
	}{
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
	}
	return result.Min(results...), errs
}

func (c *Controller) Finalize(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	stored := machine.DeepCopy()
	if !controllerutil.ContainsFinalizer(machine, v1alpha5.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provider-id", machine.Status.ProviderID))
	if err := c.cloudProvider.Delete(ctx, machine); cloudprovider.IgnoreMachineNotFoundError(err) != nil {
		return reconcile.Result{}, fmt.Errorf("terminating cloudprovider instance, %w", err)
	}
	controllerutil.RemoveFinalizer(machine, v1alpha5.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, machine) {
		if err := c.kubeClient.Patch(ctx, machine, client.MergeFrom(stored)); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("removing machine termination finalizer, %w", err)
		}
		logging.FromContext(ctx).Infof("deleted machine")
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Machine{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				node := o.(*v1.Node)
				machineList := &v1alpha5.MachineList{}
				if err := c.kubeClient.List(ctx, machineList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}, client.Limit(1)); err != nil {
					return []reconcile.Request{}
				}
				return lo.Map(machineList.Items, func(m v1alpha5.Machine, _ int) reconcile.Request {
					return reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&m),
					}
				})
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 100}))
}

func NodeForMachine(ctx context.Context, c client.Client, machine *v1alpha5.Machine) (*v1.Node, error) {
	nodeList := v1.NodeList{}
	if err := c.List(ctx, &nodeList, client.MatchingFields{"spec.providerID": machine.Status.ProviderID}, client.Limit(2)); err != nil {
		return nil, fmt.Errorf("listing nodes, %w", err)
	}
	if len(nodeList.Items) > 1 {
		return nil, &DuplicateNodeError{ProviderID: machine.Status.ProviderID}
	}
	if len(nodeList.Items) == 0 {
		return nil, &NodeNotFoundError{ProviderID: machine.Status.ProviderID}
	}
	return &nodeList.Items[0], nil
}
