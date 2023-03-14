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
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/clock"
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
	"github.com/aws/karpenter-core/pkg/controllers/machine/terminator"
	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/result"
)

type machineReconciler interface {
	Reconcile(context.Context, *v1alpha5.Machine) (reconcile.Result, error)
}

var _ corecontroller.FinalizingTypedController[*v1alpha5.Machine] = (*Controller)(nil)

// Controller is a Machine Controller
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	recorder      events.Recorder
	terminator    *terminator.Terminator

	garbageCollect *GarbageCollect
	launch         *Launch
	registration   *Registration
	initialization *Initialization
	liveness       *Liveness
}

// NewController is a constructor for the Machine Controller
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider,
	terminator *terminator.Terminator, recorder events.Recorder) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		recorder:      recorder,
		terminator:    terminator,

		garbageCollect: &GarbageCollect{kubeClient: kubeClient, cloudProvider: cloudProvider, lastChecked: cache.New(time.Minute*10, time.Second*10)},
		launch:         &Launch{kubeClient: kubeClient, cloudProvider: cloudProvider, cache: cache.New(time.Minute, time.Second*10)},
		registration:   &Registration{kubeClient: kubeClient},
		initialization: &Initialization{kubeClient: kubeClient},
		liveness:       &Liveness{clock: clk, kubeClient: kubeClient},
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
	for _, reconciler := range []machineReconciler{
		c.garbageCollect,
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
	if err := c.cleanupNodeForMachine(ctx, machine); err != nil {
		if terminator.IsNodeDrainError(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, nil
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provider-id", machine.Status.ProviderID))
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
		For(&v1alpha5.Machine{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				node := o.(*v1.Node)
				machineList := &v1alpha5.MachineList{}
				if err := c.kubeClient.List(ctx, machineList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
					return []reconcile.Request{}
				}
				return lo.Map(machineList.Items, func(m v1alpha5.Machine, _ int) reconcile.Request {
					return reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&m),
					}
				})
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 50})) // higher concurrency limit since we want fast reaction to node syncing and launch
}

func (c *Controller) cleanupNodeForMachine(ctx context.Context, machine *v1alpha5.Machine) error {
	node, err := nodeForMachine(ctx, c.kubeClient, machine)
	if err != nil {
		// We don't clean the node if we either don't find a node or have violated the single machine to single node invariant
		if IsNodeNotFoundError(err) || IsDuplicateNodeError(err) {
			return nil
		}
		return err
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", node.Name))
	if err = c.terminator.Cordon(ctx, node); err != nil {
		if terminator.IsNodeDrainError(err) {
			c.recorder.Publish(events.NodeFailedToDrain(node, err))
		}
		return fmt.Errorf("cordoning node, %w", err)
	}
	if err = c.terminator.Drain(ctx, node); err != nil {
		if terminator.IsNodeDrainError(err) {
			c.recorder.Publish(events.NodeFailedToDrain(node, err))
		}
		return fmt.Errorf("draining node, %w", err)
	}
	return nil
}

func nodeForMachine(ctx context.Context, c client.Client, machine *v1alpha5.Machine) (*v1.Node, error) {
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
