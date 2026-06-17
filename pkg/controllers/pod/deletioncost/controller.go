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

package deletioncost

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

const (
	reconcileInterval = time.Minute
	maxNodesPerCycle  = 50
)

// Controller manages pod deletion cost annotations for Karpenter-managed nodes.
// Reconcile is serialized by the singleton reconciler helper, so the
// per-controller fields below are written without explicit synchronization.
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster

	lastConsolidationState time.Time
}

// NewController creates a new pod deletion cost controller.
func NewController(
	clk clock.Clock,
	kubeClient client.Client,
	cloudProvider cloudprovider.CloudProvider,
	cluster *state.Cluster,
) *Controller {
	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		cluster:       cluster,
	}
}

// Register registers the controller with the manager
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

// Name returns the controller name
func (c *Controller) Name() string {
	return "pod.deletioncost"
}

// Reconcile reconciles the cluster state and updates pod deletion cost
// annotations. Returns errors from the constituent steps so controller-runtime
// logs them once and applies exponential backoff via the workqueue rate
// limiter; on error the operatorpkg reconciler adapter drops the RequeueAfter
// (see operatorpkg/reconciler.AsReconcilerWithRateLimiter) so the explicit
// reconcileInterval here only takes effect on the success and skip paths.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	// Defensive: the controller is also gated at registration in
	// pkg/controllers/controllers.go, so this branch only fires if the gate is
	// flipped to false at runtime.
	if !options.FromContext(ctx).FeatureGates.PodDeletionCostManagement {
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	var nodes []*state.StateNode
	for node := range c.cluster.Nodes() {
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	if c.shouldSkipUnchanged(ctx) {
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	nodePoolMap, err := c.buildNodePoolMap(ctx)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("building node pool map, %w", err)
	}

	nodeRanks, err := RankNodes(ctx, c.kubeClient, c.cluster, c.clock, nodes, nodePoolMap)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("ranking nodes, %w", err)
	}
	if len(nodeRanks) > maxNodesPerCycle {
		nodeRanks = nodeRanks[:maxNodesPerCycle]
	}

	if err := UpdatePodDeletionCosts(ctx, c.kubeClient, nodeRanks); err != nil {
		return reconciler.Result{}, fmt.Errorf("updating pod deletion costs, %w", err)
	}

	log.FromContext(ctx).V(1).WithValues("nodeCount", len(nodeRanks)).Info("updated pod deletion costs")
	return reconciler.Result{RequeueAfter: reconcileInterval}, nil
}

// shouldSkipUnchanged returns true if cluster state has not changed since the last reconcile.
// Uses the same ConsolidationState timestamp that gates the disruption controller's consolidation
// methods, ensuring this controller reacts to the same state changes that trigger consolidation.
func (c *Controller) shouldSkipUnchanged(ctx context.Context) bool {
	currentState := c.cluster.ConsolidationState()
	if currentState.Equal(c.lastConsolidationState) {
		log.FromContext(ctx).V(1).Info("no changes detected, skipping pod deletion cost update")
		reconcileSkippedTotal.Add(1, noLabels)
		return true
	}
	c.lastConsolidationState = currentState
	return false
}

// buildNodePoolMap lists all managed NodePools and returns a map keyed by name.
func (c *Controller) buildNodePoolMap(ctx context.Context) (map[string]*v1.NodePool, error) {
	nodePools, err := nodepoolutils.ListManaged(ctx, c.kubeClient, c.cloudProvider)
	if err != nil {
		return nil, fmt.Errorf("listing node pools, %w", err)
	}
	return lo.SliceToMap(nodePools, func(np *v1.NodePool) (string, *v1.NodePool) { return np.Name, np }), nil
}
