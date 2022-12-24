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

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/node"
	"github.com/aws/karpenter-core/pkg/utils/resources"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Controller is a Machine Controller
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
	return "machine"
}

func (c *Controller) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(machine, v1alpha5.TerminationFinalizer) {
		controllerutil.AddFinalizer(machine, v1alpha5.TerminationFinalizer)
	}

	found, err := c.cloudProvider.Get(ctx, machine)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting machine, %w", err)
	}
	if !found {
		// If we have already launched and resolved the machine, we should terminate
		// Otherwise, we should launch since we haven't resolved this machine yet
		if machine.Status.ProviderID != "" {
			logging.FromContext(ctx).Debugf("deleting machine with no cloudprovider representation")
			if err = c.kubeClient.Delete(ctx, machine); err != nil {
				return reconcile.Result{}, fmt.Errorf("deleting machine, %w", err)
			}
			return reconcile.Result{}, nil
		}
		logging.FromContext(ctx).Debugf("launching machine")
		if _, err = c.cloudProvider.Create(ctx, machine); err != nil {
			return reconcile.Result{}, fmt.Errorf("creating machine, %w", err)
		}
		machine.StatusConditions().MarkTrue(v1alpha5.MachineCreated)
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provider-id", machine.Status.ProviderID))

	node, err := c.nodeForMachine(ctx, machine)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting node for machine, %w", err)
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", node.Name))
	machine.StatusConditions().MarkTrue(v1alpha5.MachineRegistered)

	if err = c.syncNodeLabels(ctx, machine, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("syncing node labels with machine labels, %w", err)
	}
	checkInitialized(node, machine)

	// Requeue after a short interval so we can check for machine existence at the CloudProvider
	return reconcile.Result{RequeueAfter: time.Minute}, nil
}

func (c *Controller) Finalize(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	// TODO: Add cordon and drain logic to the finalization flow

	// Delete the instance when we remove the machine
	if err := c.cloudProvider.Delete(ctx, machine); err != nil {
		return reconcile.Result{}, fmt.Errorf("deleting machine, %w", err)
	}
	controllerutil.RemoveFinalizer(machine, v1alpha5.TerminationFinalizer)
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
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}

func (c *Controller) nodeForMachine(ctx context.Context, machine *v1alpha5.Machine) (*v1.Node, error) {
	nodeList := v1.NodeList{}
	if err := c.kubeClient.List(ctx, &nodeList, client.MatchingFields{"spec.providerID": machine.Status.ProviderID}); err != nil {
		return nil, fmt.Errorf("listing nodes, %w", err)
	}
	if len(nodeList.Items) > 1 {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineRegistered, "MultipleNodesFound", "invariant violated, machine matched multiple nodes %s",
			lo.Map(nodeList.Items, func(node v1.Node, _ int) string { return node.Name }))
		return nil, nil
	}
	if len(nodeList.Items) == 0 {
		return nil, nil
	}
	return &nodeList.Items[0], nil
}

func (c *Controller) syncNodeLabels(ctx context.Context, machine *v1alpha5.Machine, node *v1.Node) error {
	stored := node.DeepCopy()
	node.Labels = lo.Assign(node.Labels, machine.Labels)
	node.Annotations = lo.Assign(node.Annotations, machine.Annotations)
	if !equality.Semantic.DeepEqual(stored, node) {
		if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return fmt.Errorf("syncing node labels, %w", err)
		}
		logging.FromContext(ctx).Debugf("synced node labels and annotations with machine")
	}
	return nil
}

// checkInitialized checks for initialization based on if:
// a) its current status is set to Ready
// b) all the startup taints have been removed from the node
// c) all extended resources have been registered
// This method handles both nil provisioners and nodes without extended resources gracefully.
func checkInitialized(n *v1.Node, machine *v1alpha5.Machine) {
	// fast checks first
	if node.GetCondition(n, v1.NodeReady).Status != v1.ConditionTrue {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "NodeNotReady", "node not ready")
	}
	if taint, ok := IsStartupTaintRemoved(n, machine); !ok {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "StartupTaintsExist", "startup taint %s still exists", taint)
	}
	if name, ok := IsExtendedResourceRegistered(n, machine); !ok {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "ExtendedResourceNotRegistered", "extended resource %s not registered", name)
	}
	machine.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
}

// IsStartupTaintRemoved returns true if there are no startup taints registered for the provisioner, or if all startup
// taints have been removed from the node
func IsStartupTaintRemoved(node *v1.Node, machine *v1alpha5.Machine) (*v1.Taint, bool) {
	if machine != nil {
		for _, startupTaint := range machine.Spec.StartupTaints {
			for i := 0; i < len(node.Spec.Taints); i++ {
				// if the node still has a startup taint applied, it's not ready
				if startupTaint.MatchTaint(&node.Spec.Taints[i]) {
					return &node.Spec.Taints[i], false
				}
			}
		}
	}
	return nil, true
}

// IsExtendedResourceRegistered returns true if there are no extended resources on the node, or they have all been
// registered by device plugins
func IsExtendedResourceRegistered(node *v1.Node, machine *v1alpha5.Machine) (v1.ResourceName, bool) {
	for resourceName, quantity := range machine.Status.Allocatable {
		if quantity.IsZero() {
			continue
		}
		// kubelet will zero out both the capacity and allocatable for an extended resource on startup, so if our
		// annotation says the resource should be there, but it's zero'd in both then the device plugin hasn't
		// registered it yet.
		// We wait on allocatable since this is the value that is used in scheduling
		if resources.IsZero(node.Status.Allocatable[resourceName]) {
			return resourceName, false
		}
	}
	return "", true
}
