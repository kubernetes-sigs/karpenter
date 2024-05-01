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

package informer

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// NodePoolController reconciles NodePools to re-trigger consolidation on change.
type NodePoolController struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

func NewNodePoolController(kubeClient client.Client, cluster *state.Cluster) *NodePoolController {
	return &NodePoolController{
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

func (c *NodePoolController) Reconcile(ctx context.Context, np *v1beta1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "state.nodepool") //nolint:ineffassign,staticcheck

func (c *NodePoolController) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(c.Name()).With("nodepool", nodePool.Name))
	np := &v1beta1.NodePool{}
	if err := c.kubeClient.Get(ctx, client.ObjectKeyFromObject(nodePool), np); err != nil {
		if errors.IsNotFound(err) {
			// notify cluster state of the node deletion
			c.cluster.DeleteNodePool(nodePool.Name)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	c.cluster.UpdateNodePool(nodePool)
	// Something changed in the NodePool so we should re-consider consolidation
	c.cluster.MarkUnconsolidated()
	return reconcile.Result{}, nil
}

func (c *NodePoolController) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("state.nodepool").
		For(&v1beta1.NodePool{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithEventFilter(predicate.Funcs{DeleteFunc: func(event event.DeleteEvent) bool { return false }}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
