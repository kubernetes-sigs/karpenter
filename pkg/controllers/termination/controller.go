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

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/machine/terminator"
	terminatorevents "github.com/aws/karpenter-core/pkg/controllers/machine/terminator/events"
	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

var _ corecontroller.FinalizingTypedController[*v1.Node] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
	terminator *terminator.Terminator
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
	// If there is no longer a machine for this node, remove the finalizer and delete the node
	if len(machineList.Items) == 0 {
		// TODO @joinnis: Remove this section after v1beta1 migration is completed
		// We need to keep the full termination flow in here during the migration timeframe
		// This is because there is a short time where a node with the karpenter.sh/termination finalizer
		// may not have a machine owner and we should still terminate gracefully
		if err := c.terminator.Cordon(ctx, node); err != nil {
			if terminator.IsNodeDrainError(err) {
				c.recorder.Publish(terminatorevents.NodeFailedToDrain(node, err))
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, fmt.Errorf("cordoning node, %w", err)
		}
		if err := c.terminator.Drain(ctx, node); err != nil {
			return reconcile.Result{}, fmt.Errorf("draining node, %w", err)
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
	for i := range machineList.Items {
		if err := c.kubeClient.Delete(ctx, &machineList.Items[i]); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1.Node{}).
		Watches(
			&source.Kind{Type: &v1alpha5.Machine{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				machine := o.(*v1alpha5.Machine)
				nodeList := &v1.NodeList{}
				if machine.Status.ProviderID == "" {
					return nil
				}
				if err := c.kubeClient.List(ctx, nodeList, client.MatchingFields{"spec.providerID": machine.Status.ProviderID}); err != nil {
					return nil
				}
				return lo.Map(nodeList.Items, func(n v1.Node, _ int) reconcile.Request {
					return reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&n),
					}
				})
			}),
		).
		WithOptions(
			controller.Options{
				MaxConcurrentReconciles: 10,
			},
		))
}
