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
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
	"github.com/aws/karpenter-core/pkg/utils/result"
)

type nodeClaimReconciler interface {
	Reconcile(context.Context, *v1beta1.NodePool, *v1beta1.NodeClaim) (reconcile.Result, error)
}

// Controller is a disruption controller that adds StatusConditions to Machines when they meet certain disruption conditions
// e.g. When the NodeClaim has surpassed its owning provisioner's expirationTTL, then it is marked as "Expired" in the StatusConditions
type Controller struct {
	kubeClient client.Client

	drift      *Drift
	expiration *Expiration
	emptiness  *Emptiness
}

// NewController constructs a machine disruption controller
func NewController(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		drift:      &Drift{cloudProvider: cloudProvider},
		expiration: &Expiration{kubeClient: kubeClient, clock: clk},
		emptiness:  &Emptiness{kubeClient: kubeClient, cluster: cluster, clock: clk},
	}
}

// Reconcile executes a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	stored := nodeClaim.DeepCopy()
	nodePool, err := nodeclaimutil.Owner(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	var results []reconcile.Result
	var errs error
	reconcilers := []nodeClaimReconciler{
		c.expiration,
		c.drift,
		c.emptiness,
	}
	for _, reconciler := range reconcilers {
		res, err := reconciler.Reconcile(ctx, nodePool, nodeClaim)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}
	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		if err = nodeclaimutil.UpdateStatus(ctx, c.kubeClient, nodeClaim); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return result.Min(results...), errs
}

var _ corecontroller.TypedController[*v1beta1.NodeClaim] = (*NodeClaimController)(nil)

type NodeClaimController struct {
	*Controller
}

func NewNodeClaimController(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1beta1.NodeClaim](kubeClient, &NodeClaimController{
		Controller: NewController(clk, kubeClient, cluster, cloudProvider),
	})
}

func (c *NodeClaimController) Name() string {
	return "nodeclaim.disruption"
}

func (c *NodeClaimController) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	return c.Controller.Reconcile(ctx, nodeClaim)
}

func (c *NodeClaimController) Builder(ctx context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodeClaim{}, builder.WithPredicates(
			predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.Funcs{
					UpdateFunc: func(e event.UpdateEvent) bool {
						oldNodeClaim := e.ObjectOld.(*v1beta1.NodeClaim)
						newNodeClaim := e.ObjectNew.(*v1beta1.NodeClaim)

						// One of the status conditions that affects disruption has changed
						// which means that we should re-consider this for disruption
						for _, cond := range v1beta1.LivingConditions {
							if !equality.Semantic.DeepEqual(
								oldNodeClaim.StatusConditions().GetCondition(cond),
								newNodeClaim.StatusConditions().GetCondition(cond),
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
			&v1beta1.NodePool{},
			nodeclaimutil.NodePoolEventHandler(ctx, c.kubeClient),
		).
		Watches(
			&v1.Node{},
			nodeclaimutil.NodeEventHandler(ctx, c.kubeClient),
		).
		Watches(
			&v1.Pod{},
			nodeclaimutil.PodEventHandler(ctx, c.kubeClient),
		),
	)
}

var _ corecontroller.TypedController[*v1alpha5.Machine] = (*MachineController)(nil)

type MachineController struct {
	*Controller
}

func NewMachineController(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &MachineController{
		Controller: NewController(clk, kubeClient, cluster, cloudProvider),
	})
}

func (c *MachineController) Name() string {
	return "machine.disruption"
}

func (c *MachineController) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	return c.Controller.Reconcile(ctx, nodeclaimutil.New(machine))
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
			&v1alpha5.Provisioner{},
			machineutil.ProvisionerEventHandler(ctx, c.kubeClient),
		).
		Watches(
			&v1.Node{},
			machineutil.NodeEventHandler(ctx, c.kubeClient),
		).
		Watches(
			&v1.Pod{},
			machineutil.PodEventHandler(ctx, c.kubeClient),
		),
	)
}
