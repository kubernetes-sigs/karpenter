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

package counter

import (
	"context"
	"time"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"

	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/utils/resources"
)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

// NewController is a constructor
func NewController(kubeClient client.Client, cluster *state.Cluster) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

// Reconcile a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool) (reconcile.Result, error) {
	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// Otherwise, we have the potential to patch over the status with a lower value for the provisioner resource
	// counts on startup
	if !c.cluster.Synced(ctx) {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	stored := nodePool.DeepCopy()
	// Determine resource usage and update provisioner.status.resources
	nodePool.Status.Resources = c.resourceCountsFor(lo.Ternary(nodePool.IsProvisioner, v1alpha5.ProvisionerNameLabelKey, v1beta1.NodePoolLabelKey), nodePool.Name)
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		if err := nodepoolutil.PatchStatus(ctx, c.kubeClient, stored, nodePool); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) resourceCountsFor(ownerLabel string, ownerName string) v1.ResourceList {
	var res v1.ResourceList
	// Record all resources provisioned by the provisioners, we look at the cluster state nodes as their capacity
	// is accurately reported even for nodes that haven't fully started yet. This allows us to update our provisioner
	// status immediately upon node creation instead of waiting for the node to become ready.
	c.cluster.ForEachNode(func(n *state.StateNode) bool {
		// Don't count nodes that we are planning to delete. This is to ensure that we are consistent throughout
		// our provisioning and deprovisioning loops
		if n.MarkedForDeletion() {
			return true
		}
		if n.Labels()[ownerLabel] == ownerName {
			res = resources.MergeInto(res, n.Capacity())
		}
		return true
	})
	return functional.FilterMap(res, func(_ v1.ResourceName, v resource.Quantity) bool { return !v.IsZero() })
}

type NodePoolController struct {
	*Controller
}

func NewNodePoolController(kubeClient client.Client, cluster *state.Cluster) corecontroller.Controller {
	return corecontroller.Typed[*v1beta1.NodePool](kubeClient, &NodePoolController{
		Controller: NewController(kubeClient, cluster),
	})
}

func (c *NodePoolController) Name() string {
	return "nodepool.counter"
}

func (c *NodePoolController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodePool{}).
		Watches(
			&v1beta1.NodeClaim{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
				if name, ok := o.GetLabels()[v1beta1.NodePoolLabelKey]; ok {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}),
		).
		Watches(
			&v1.Node{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
				if name, ok := o.GetLabels()[v1beta1.NodePoolLabelKey]; ok {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}

type ProvisionerController struct {
	*Controller
}

func NewProvisionerController(kubeClient client.Client, cluster *state.Cluster) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Provisioner](kubeClient, &ProvisionerController{
		Controller: NewController(kubeClient, cluster),
	})
}

func (c *ProvisionerController) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner) (reconcile.Result, error) {
	return c.Controller.Reconcile(ctx, nodepoolutil.New(provisioner))
}

func (c *ProvisionerController) Name() string {
	return "counter"
}

func (c *ProvisionerController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Provisioner{}).
		Watches(
			&v1alpha5.Machine{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
				if name, ok := o.GetLabels()[v1alpha5.ProvisionerNameLabelKey]; ok {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}),
		).
		Watches(
			&v1.Node{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
				if name, ok := o.GetLabels()[v1alpha5.ProvisionerNameLabelKey]; ok {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}
