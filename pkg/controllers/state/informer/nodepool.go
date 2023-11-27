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

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
)

var _ operatorcontroller.TypedController[*v1beta1.NodePool] = (*NodePoolController)(nil)

// NodePoolController reconciles NodePools to re-trigger consolidation on change.
type NodePoolController struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

func NewNodePoolController(kubeClient client.Client, cluster *state.Cluster) operatorcontroller.Controller {
	return operatorcontroller.Typed[*v1beta1.NodePool](kubeClient, &NodePoolController{
		kubeClient: kubeClient,
		cluster:    cluster,
	})
}

func (c *NodePoolController) Name() string {
	return "state.nodepool"
}

func (c *NodePoolController) Reconcile(_ context.Context, _ *v1beta1.NodePool) (reconcile.Result, error) {
	// Something changed in the NodePool so we should re-consider consolidation
	c.cluster.MarkUnconsolidated()
	return reconcile.Result{}, nil
}

func (c *NodePoolController) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodePool{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithEventFilter(predicate.Funcs{DeleteFunc: func(event event.DeleteEvent) bool { return false }}),
	)
}
