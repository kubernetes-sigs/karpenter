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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter-core/pkg/utils/node"
	"github.com/aws/karpenter-core/pkg/utils/resources"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha1"
)

// Controller is a Machine Controller
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController is a constructor for the Machine Controller
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha1.Machine](kubeClient, &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	})
}

func (*Controller) Name() string {
	return "machine"
}

//nolint:gocyclo
func (c *Controller) Reconcile(ctx context.Context, machine *v1alpha1.Machine) (reconcile.Result, error) {
	if cond := machine.StatusConditions().GetCondition(v1alpha1.MachineCreated); cond.Status == v1.ConditionFalse {
		if _, err := c.cloudProvider.Create(ctx, machine); err != nil {
			return reconcile.Result{}, fmt.Errorf("creating instance %s, %w", machine.Name, err)
		}
		machine.StatusConditions().MarkTrue(v1alpha1.MachineCreated)
	}

	// Get Node
	nodeList := v1.NodeList{}
	if err := c.kubeClient.List(ctx, &nodeList, client.MatchingLabels{v1alpha5.MachineNameLabelKey: machine.Name}); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodes, %w", err)
	}
	if len(nodeList.Items) > 1 {
		machine.StatusConditions().MarkFalse(v1alpha1.MachineRegistered, "MultipleNodesFound", "invariant violated, machine matched multiple nodes %s",
			lo.Map(nodeList.Items, func(node v1.Node, _ int) string { return node.Name }))
		return reconcile.Result{}, nil
	}
	if len(nodeList.Items) == 0 {
		return reconcile.Result{}, nil
	}
	n := nodeList.Items[0]
	machine.StatusConditions().MarkTrue(v1alpha1.MachineRegistered)

	provisionerKey, ok := machine.Owner()
	if !ok {
		machine.StatusConditions().MarkFalse(v1alpha1.MachineHealthy, "ProvisionerOwnerNotFound", "expected provisioner owner reference to exist on the machine")
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, provisionerKey, provisioner); err != nil {
		if errors.IsNotFound(err) {
			machine.StatusConditions().MarkFalse(v1alpha1.MachineHealthy, "ProvisionerOwnerNotFound", "expected provisioner %s to exist on the cluster", provisionerKey.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting provisioner owner, %w", err)
	}
	instanceTypeName, ok := machine.Labels[v1.LabelInstanceType]
	if !ok {
		machine.StatusConditions().MarkFalse(v1alpha1.MachineHealthy, "InstanceTypeNotFound", "expected instance type label %s to exist on the machine", v1.LabelInstanceType)
		return reconcile.Result{}, nil
	}
	instanceTypes, err := c.cloudProvider.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting instance types, %w", err)
	}
	instanceType, ok := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
		return i.Name == instanceTypeName
	})
	if !ok {
		machine.StatusConditions().MarkFalse(v1alpha1.MachineHealthy, "InstanceTypeNotFound", "expected instance type %s to exist on the cloudprovider", instanceTypeName)
	}
	machine.StatusConditions().MarkTrue(v1alpha1.MachineHealthy)

	// Check initialized
	if !IsInitialized(&n, machine, instanceType) {
		return reconcile.Result{}, nil
	}
	machine.StatusConditions().MarkTrue(v1alpha1.MachineInitialized)
	return reconcile.Result{}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha1.Machine{}).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				if name, ok := o.GetLabels()[v1alpha5.MachineNameLabelKey]; ok {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}

// IsInitialized returns true if the node has:
// a) its current status is set to Ready
// b) all the startup taints have been removed from the node
// c) all extended resources have been registered
// This method handles both nil provisioners and nodes without extended resources gracefully.
func IsInitialized(n *v1.Node, machine *v1alpha1.Machine, instanceType *cloudprovider.InstanceType) bool {
	// fast checks first
	if node.GetCondition(n, v1.NodeReady).Status != v1.ConditionTrue {
		machine.StatusConditions().MarkFalse(v1alpha1.MachineInitialized, "NodeNotReady", "node not ready")
		return false
	}
	if keyValue, ok := DoLabelsMatch(n, machine); !ok {
		machine.StatusConditions().MarkFalse(v1alpha1.MachineInitialized, "LabelMismatch", "expected node to have label %s=%s", keyValue.First, keyValue.Second)
	}
	if taint, ok := IsStartupTaintRemoved(n, machine); !ok {
		machine.StatusConditions().MarkFalse(v1alpha1.MachineInitialized, "StartupTaintsExist", "startup taint %s still exists", taint)
		return false
	}
	if name, ok := IsExtendedResourceRegistered(n, instanceType); !ok {
		machine.StatusConditions().MarkFalse(v1alpha1.MachineInitialized, "ExtendedResourceNotRegistered", "extended resource %s not registered", name)
		return false
	}
	return true
}

func DoLabelsMatch(node *v1.Node, machine *v1alpha1.Machine) (functional.Pair[string, string], bool) {
	for key, expected := range machine.Labels {
		if actual, ok := node.Labels[key]; !ok {
			return functional.Pair[string, string]{First: key}, false
		} else if actual != expected {
			return functional.Pair[string, string]{First: key, Second: expected}, false
		}
	}
	return functional.Pair[string, string]{}, true
}

// IsStartupTaintRemoved returns true if there are no startup taints registered for the provisioner, or if all startup
// taints have been removed from the node
func IsStartupTaintRemoved(node *v1.Node, machine *v1alpha1.Machine) (*v1.Taint, bool) {
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
func IsExtendedResourceRegistered(node *v1.Node, instanceType *cloudprovider.InstanceType) (v1.ResourceName, bool) {
	if instanceType == nil {
		// no way to know, so assume they're registered
		return "", true
	}
	for resourceName, quantity := range instanceType.Capacity {
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
