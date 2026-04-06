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
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	utilscontroller "sigs.k8s.io/karpenter/pkg/utils/controller"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

// NodePoolController reconciles NodePools to re-trigger consolidation on change.
type NodePoolController struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster
	clusterCost   *cost.ClusterCost
}

func NewNodePoolController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster, clusterCost *cost.ClusterCost) *NodePoolController {
	return &NodePoolController{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		cluster:       cluster,
		clusterCost:   clusterCost,
	}
}

func (c *NodePoolController) Name() string {
	return "state.nodepool"
}

func (c *NodePoolController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name()) //nolint:ineffassign,staticcheck

	np := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, np); err != nil {
		if errors.IsNotFound(err) {
			// notify cluster state of the nodepool deletion
			c.clusterCost.DeleteNodePool(ctx, req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nodepoolutils.IsManaged(np, c.cloudProvider) {
		return reconcile.Result{}, nil
	}

	// Something changed in the NodePool so we should re-consider consolidation
	c.cluster.MarkUnconsolidated()
	return reconcile.Result{}, nil
}

func (c *NodePoolController) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.NodePool{}, builder.WithPredicates(nodepoolutils.IsManagedPredicateFuncs(c.cloudProvider))).
		WithOptions(controller.Options{MaxConcurrentReconciles: utilscontroller.LinearScaleReconciles(utilscontroller.CPUCount(ctx), minReconciles, maxReconciles)}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(c)
}
