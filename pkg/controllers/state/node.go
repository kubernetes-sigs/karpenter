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

package state

import (
	"context"
	"net/http"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

const nodeControllerName = "node-state"

var _ corecontroller.TypedControllerWithDeletion[*v1.Node] = (*NodeController)(nil)
var _ corecontroller.TypedControllerWithHealthCheck[*v1.Node] = (*NodeController)(nil)

// NodeController reconciles nodes for the purpose of maintaining state regarding nodes that is expensive to compute.
type NodeController struct {
	kubeClient client.Client
	cluster    *Cluster
}

// NewNodeController constructs a controller instance
func NewNodeController(kubeClient client.Client, cluster *Cluster) corecontroller.Controller {
	return corecontroller.For[*v1.Node](kubeClient, &NodeController{
		kubeClient: kubeClient,
		cluster:    cluster,
	})
}

func (c *NodeController) Reconcile(ctx context.Context, node *v1.Node) (*v1.Node, reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(nodeControllerName).With("node", node.Name))
	if err := c.cluster.updateNode(ctx, node); err != nil {
		return nil, reconcile.Result{}, err
	}
	// ensure it's aware of any nodes we discover, this is a no-op if the node is already known to our cluster state
	return nil, reconcile.Result{Requeue: true, RequeueAfter: stateRetryPeriod}, nil
}

func (c *NodeController) OnDeleted(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
	// notify cluster state of the node deletion
	c.cluster.deleteNode(req.Name)
	return reconcile.Result{}, nil
}

func (c *NodeController) LivenessProbe(req *http.Request) error {
	// node state controller can't really fail, but we use it to check on the cluster state
	return c.cluster.LivenessProbe(req)
}

func (c *NodeController) Builder(_ context.Context, m manager.Manager) corecontroller.TypedBuilder {
	return corecontroller.NewTypedBuilderAdapter(controllerruntime.
		NewControllerManagedBy(m).
		Named(nodeControllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}
