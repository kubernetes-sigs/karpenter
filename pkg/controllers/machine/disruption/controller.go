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

package disruption

import (
	"context"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	"github.com/aws/karpenter-core/pkg/utils/result"
)

type machineReconciler interface {
	Reconcile(context.Context, *v1alpha5.Provisioner, *v1alpha5.Machine) (reconcile.Result, error)
}

var _ corecontroller.TypedController[*v1alpha5.Machine] = (*Controller)(nil)

// Controller is a disruption controller that adds StatusConditions to Machines when they meet certain disruption conditions
// e.g. When the Machine has surpassed its owning provisioner's expirationTTL, then it is marked as "Expired" in the StatusConditions
type Controller struct {
	kubeClient client.Client

	drift      *Drift
	expiration *Expiration
	emptiness  *Emptiness
}

// NewController constructs a machine disruption controller
func NewController(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &Controller{
		kubeClient: kubeClient,
		drift:      &Drift{cloudProvider: cloudProvider},
		expiration: &Expiration{kubeClient: kubeClient, clock: clk},
		emptiness:  &Emptiness{kubeClient: kubeClient, cluster: cluster, clock: clk},
	})
}

func (c *Controller) Name() string {
	return "machine.disruption"
}

// Reconcile executes a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	stored := machine.DeepCopy()
	if _, ok := machine.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok {
		return reconcile.Result{}, nil
	}
	if !machine.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: machine.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	var results []reconcile.Result
	var errs error
	reconcilers := []machineReconciler{
		c.expiration,
		c.drift,
		c.emptiness,
	}
	for _, reconciler := range reconcilers {
		res, err := reconciler.Reconcile(ctx, provisioner, machine)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}
	if !equality.Semantic.DeepEqual(stored, machine) {
		if err := c.kubeClient.Status().Update(ctx, machine); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return result.Min(results...), errs
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Machine{}, builder.WithPredicates(
			predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.Funcs{
					UpdateFunc: func(e event.UpdateEvent) bool {
						oldMachine := e.ObjectOld.(*v1alpha5.Machine)
						newMachine := e.ObjectNew.(*v1alpha5.Machine)

						// One of the status conditions that affects disruption has changed
						// which means that we should re-consider this for disruption
						for _, cond := range v1alpha5.LivingConditions {
							if !equality.Semantic.DeepEqual(
								oldMachine.StatusConditions().GetCondition(cond),
								newMachine.StatusConditions().GetCondition(cond),
							) {
								return true
							}
						}
						return false
					},
				},
			),
		)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Watches(
			&source.Kind{Type: &v1alpha5.Provisioner{}},
			machineutil.ProvisionerEventHandler(ctx, c.kubeClient),
		).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			machineutil.NodeEventHandler(ctx, c.kubeClient),
		).
		Watches(
			&source.Kind{Type: &v1.Pod{}},
			machineutil.PodEventHandler(ctx, c.kubeClient),
		),
	)
}
