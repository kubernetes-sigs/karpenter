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

package informer

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
)

// NodeClaimController reconciles machine for the purpose of maintaining state.
type NodeClaimController struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

// NewNodeClaimController constructs a controller instance
func NewNodeClaimController(kubeClient client.Client, cluster *state.Cluster) operatorcontroller.Controller {
	return &NodeClaimController{
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

func (c *NodeClaimController) Name() string {
	return "state.nodeclaim"
}

func (c *NodeClaimController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(c.Name()).With("nodeclaim", req.NamespacedName.Name))
	nodeClaim := &v1beta1.NodeClaim{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, nodeClaim); err != nil {
		if errors.IsNotFound(err) {
			// notify cluster state of the node deletion
			c.cluster.DeleteNodeClaim(req.Name)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	c.cluster.UpdateNodeClaim(nodeClaim)
	// ensure it's aware of any nodes we discover, this is a no-op if the node is already known to our cluster state
	return reconcile.Result{RequeueAfter: stateRetryPeriod}, nil
}

func (c *NodeClaimController) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodeClaim{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}
