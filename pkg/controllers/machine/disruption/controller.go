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

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
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

type machineReconciler interface {
	Reconcile(context.Context, *v1alpha5.Provisioner, *v1alpha5.Machine) (reconcile.Result, error)
}

var _ corecontroller.TypedController[*v1alpha5.Machine] = (*Controller)(nil)

// Controller is a disruption controller that adds StatusConditions to Machines when they meet the
// disruption criteria for that disruption type
// e.g. When the Machine has surpassed its owning provisioner's expirationTTL, then it is marked as "VoluntarilyDisrupted"
// in the StatusConditions with "Expired" as the reason
type Controller struct {
	kubeClient client.Client

	drift      *Drift
	expiration *Expiration
	emptiness  *Emptiness
}

// NewController constructs a nodeController instance
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &Controller{
		kubeClient: kubeClient,
		drift:      &Drift{cloudProvider: cloudProvider},
		expiration: &Expiration{kubeClient: kubeClient, clock: clk},
		emptiness:  &Emptiness{kubeClient: kubeClient, clock: clk},
	})
}

func (c *Controller) Name() string {
	return "machine.disruption"
}

// Reconcile executes a reallocation control loop for the resource
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
		For(&v1alpha5.Machine{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Watches(
			// Reconcile all machines related to a provisioner when it changes.
			&source.Kind{Type: &v1alpha5.Provisioner{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				machineList := &v1alpha5.MachineList{}
				if err := c.kubeClient.List(ctx, machineList, client.MatchingLabels(map[string]string{v1alpha5.ProvisionerNameLabelKey: o.GetName()})); err != nil {
					logging.FromContext(ctx).Errorf("Failed to list machines when mapping watch events, %s", err)
					return requests
				}
				return lo.Map(machineList.Items, func(machine v1alpha5.Machine, _ int) reconcile.Request {
					return reconcile.Request{NamespacedName: types.NamespacedName{Name: machine.Name}}
				})
			}),
		).
		Watches(
			// Reconcile machine when a pod assigned to an associated node changes.
			&source.Kind{Type: &v1.Pod{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				if name := o.(*v1.Pod).Spec.NodeName; name != "" {
					node := &v1.Node{}
					if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
						if !errors.IsNotFound(err) {
							logging.FromContext(ctx).Errorf("failed to get node when mapping expiration watch events, %s", err)
						}
						return requests
					}
					machineList := &v1alpha5.MachineList{}
					if err := c.kubeClient.List(ctx, machineList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
						logging.FromContext(ctx).Errorf("Failed to list machines when mapping watch events, %s", err)
						return requests
					}
					return lo.Map(machineList.Items, func(machine v1alpha5.Machine, _ int) reconcile.Request {
						return reconcile.Request{NamespacedName: types.NamespacedName{Name: machine.Name}}
					})
				}
				return requests
			}),
		))
}
