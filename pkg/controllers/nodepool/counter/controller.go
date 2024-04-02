/*
Copyright The Kubernetes Authors.

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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/utils/functional"

	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

var _ operatorcontroller.TypedController[*v1beta1.NodePool] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

// NewController is a constructor
func NewController(kubeClient client.Client, cluster *state.Cluster) operatorcontroller.Controller {
	return operatorcontroller.Typed[*v1beta1.NodePool](kubeClient, &Controller{
		kubeClient: kubeClient,
		cluster:    cluster,
	})
}

// Reconcile a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool) (reconcile.Result, error) {
	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// Otherwise, we have the potential to patch over the status with a lower value for the nodepool resource
	// counts on startup
	if !c.cluster.Synced(ctx) {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	stored := nodePool.DeepCopy()
	// Determine resource usage and update nodepool.status.resources
	nodePool.Status.Resources = c.resourceCountsFor(v1beta1.NodePoolLabelKey, nodePool.Name)
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		if err := c.kubeClient.Status().Patch(ctx, nodePool, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) resourceCountsFor(ownerLabel string, ownerName string) v1.ResourceList {
	var res v1.ResourceList
	var nodeCount int64
	// Record all resources provisioned by the nodepools, we look at the cluster state nodes as their capacity
	// is accurately reported even for nodes that haven't fully started yet. This allows us to update our nodepool
	// status immediately upon node creation instead of waiting for the node to become ready.
	c.cluster.ForEachNode(func(n *state.StateNode) bool {
		// Don't count nodes that we are planning to delete. This is to ensure that we are consistent throughout
		// our provisioning and deprovisioning loops
		if n.MarkedForDeletion() {
			return true
		}
		if n.Labels()[ownerLabel] == ownerName {
			res = resources.MergeInto(res, n.Capacity())
			nodeCount++
		}
		return true
	})

	// Add node count to NodePool resources
	resMap := make(map[v1.ResourceName]resource.Quantity)
	resMap["nodes"] = *resource.NewQuantity(nodeCount, resource.DecimalSI)
	res = resources.MergeInto(res, v1.ResourceList(resMap))

	return functional.FilterMap(res, func(_ v1.ResourceName, v resource.Quantity) bool { return !v.IsZero() })
}

func (c *Controller) Name() string {
	return "nodepool.counter"
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(controllerruntime.
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
