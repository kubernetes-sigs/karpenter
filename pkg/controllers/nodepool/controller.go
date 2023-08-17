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

package nodepool

import (
	"context"
	"fmt"
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/clock"

	"github.com/aws/karpenter-core/pkg/controllers/state"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/functional"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	nodepoolutils "github.com/aws/karpenter-core/pkg/utils/nodepool"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

var _ corecontroller.TypedController[*v1beta1.NodePool] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
	cluster    *state.Cluster
	clock      clock.Clock
}

// NewController creates a new nodepool status controller.
func NewController(kubeClient client.Client, cluster *state.Cluster, clock clock.Clock) corecontroller.Controller {
	return corecontroller.Typed[*v1beta1.NodePool](kubeClient, &Controller{
		kubeClient: kubeClient,
		cluster:    cluster,
		clock:      clock,
	})
}

func (c *Controller) Name() string {
	return "nodepool/status"
}

// Reconcile a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool) (reconcile.Result, error) {
	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// Otherwise, we have the potential to patch over the status with a lower value for the nodePool resource
	// counts on startup
	if !c.cluster.Synced(ctx) {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	stored := nodePool.DeepCopy()
	// Determine resource usage and update nodePool.status.resources
	var nodeCount, currentDisruptions int64
	nodePool.Status.Resources, nodeCount, currentDisruptions = c.resourceCountsFor(nodePool.Name)
	active, totalAllowed, err := nodepoolutils.ParseSchedule(nodePool.Spec.Deprovisioning.Schedule, c.clock.Now(), nodeCount)
	if err != nil {
		return fmt.Errorf("parsing schedule, %w", err)
	}
	// If the allowedDisruptions doesn't have a limit, return the number of nodes in the provisioner.
	if !active || totalAllowed == math.MaxInt{
		nodePool.Status.AllowedDisruptions == &nodeCount
	// Otherwise, return the total allowed minus the currentDisruptions.
	} else {
		nodePool.Status.AllowedDisruptions = totalAllowed - currentDisruptions
	}
	nodePool.Status.CurrentDisruptions = currentDisruptions
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		if err := c.kubeClient.Status().Patch(ctx, nodePool, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) resourceCountsFor(nodePoolName string) (v1.ResourceList, int64, int64) {
	var res v1.ResourceList
	var nodes int64 = 0
	var deleting int64 = 0
	// Record all resources provisioned by the nodePools, we look at the cluster state nodes as their capacity
	// is accurately reported even for nodes that haven't fully started yet. This allows us to update our nodePool
	// status immediately upon node creation instead of waiting for the node to become ready.
	c.cluster.ForEachNode(func(n *state.StateNode) bool {
		if n.Labels()[v1beta1.NodePoolLabelKey] == nodePoolName {
			nodes++
			// Don't count nodes that we are planning to delete. This is to ensure that we are consistent throughout
			// our provisioning and deprovisioning loops
			if n.MarkedForDeletion() {
				deleting++
				return true
			}
			res = resources.Merge(res, n.Capacity())
		}
		return true
	})
	return functional.FilterMap(res, func(_ v1.ResourceName, v resource.Quantity) bool { return !v.IsZero() }), nodes, deleting
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodePool{}).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				if name, ok := o.GetLabels()[v1beta1.NodePoolLabelKey]; ok {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}
