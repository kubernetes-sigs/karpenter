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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
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

	"github.com/aws/karpenter-core/pkg/utils/resources"
)

var _ corecontroller.TypedController[*v1alpha5.Provisioner] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

// NewController is a constructor
func NewController(kubeClient client.Client, cluster *state.Cluster) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Provisioner](kubeClient, &Controller{
		kubeClient: kubeClient,
		cluster:    cluster,
	})
}

func (c *Controller) Name() string {
	return "counter"
}

// Reconcile a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner) (reconcile.Result, error) {
	stored := provisioner.DeepCopy()
	// Determine resource usage and update provisioner.status.resources
	provisioner.Status.Resources = c.resourceCountsFor(provisioner.Name)
	if !equality.Semantic.DeepEqual(stored, provisioner) {
		if err := c.kubeClient.Status().Patch(ctx, provisioner, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) resourceCountsFor(provisionerName string) v1.ResourceList {
	var res v1.ResourceList
	// Record all resources provisioned by the provisioners, we look at the cluster state nodes as their capacity
	// is accurately reported even for nodes that haven't fully started yet. This allows us to update our provisioner
	// status immediately upon node creation instead of waiting for the node to become ready.
	c.cluster.ForEachNode(func(n *state.Node) bool {
		// Don't count nodes that we are planning to delete. This is to ensure that we are consistent throughout
		// our provisioning and deprovisioning loops
		if n.MarkedForDeletion() {
			return true
		}
		if n.Labels()[v1alpha5.ProvisionerNameLabelKey] == provisionerName {
			res = resources.Merge(res, n.Capacity())
		}
		return true
	})
	return functional.FilterMap(res, func(_ v1.ResourceName, v resource.Quantity) bool { return !v.IsZero() })
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Provisioner{}).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				if name, ok := o.GetLabels()[v1alpha5.ProvisionerNameLabelKey]; ok {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}
