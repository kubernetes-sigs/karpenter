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
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

const (
	reconcileInterval = time.Minute
	maxNodesPerCycle  = 50
)

// Controller manages pod deletion cost annotations for Karpenter-managed nodes
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster
	recorder      events.Recorder

	rankingEngine          *RankingEngine
	annotationMgr          *AnnotationManager
	lastConsolidationState time.Time
	previouslyLabeledNodes map[string]bool
}

// NewController creates a new pod deletion cost controller
func NewController(
	clk clock.Clock,
	kubeClient client.Client,
	cloudProvider cloudprovider.CloudProvider,
	cluster *state.Cluster,
	recorder events.Recorder,
) *Controller {
	return &Controller{
		clock:                  clk,
		kubeClient:             kubeClient,
		cloudProvider:          cloudProvider,
		cluster:                cluster,
		recorder:               recorder,
		rankingEngine:          NewRankingEngine(),
		annotationMgr:          NewAnnotationManager(kubeClient, recorder),
		previouslyLabeledNodes: make(map[string]bool),
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

// Reconcile reconciles the cluster state and updates pod deletion cost annotations
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	opts := options.FromContext(ctx)

	if !opts.FeatureGates.PodDeletionCostManagement {
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

	nodeRanks, err := c.rankingEngine.RankNodes(ctx, c.kubeClient, nodes)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to rank nodes")
		c.recorder.Publish(DisabledEvent(fmt.Sprintf("failed to rank nodes: %v", err)))
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	c.recorder.Publish(RankingCompletedEvent(len(nodeRanks)))

	activeRanks := c.boundAndCleanup(ctx, nodeRanks)

	if err := c.annotationMgr.UpdatePodDeletionCosts(ctx, activeRanks); err != nil {
		log.FromContext(ctx).Error(err, "failed to update pod deletion costs")
		c.recorder.Publish(DisabledEvent(fmt.Sprintf("failed to update pod deletion costs: %v", err)))
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	log.FromContext(ctx).V(1).WithValues("nodeCount", len(activeRanks)).Info("updated pod deletion costs")

	return reconciler.Result{RequeueAfter: reconcileInterval}, nil
}

// shouldSkipUnchanged returns true if cluster state has not changed since the last reconcile.
// Uses the same ConsolidationState timestamp that gates the disruption controller's consolidation
// methods, ensuring this controller reacts to the same state changes that trigger consolidation.
func (c *Controller) shouldSkipUnchanged(ctx context.Context) bool {
	currentState := c.cluster.ConsolidationState()
	if currentState.Equal(c.lastConsolidationState) {
		log.FromContext(ctx).V(1).Info("no changes detected, skipping pod deletion cost update")
		SkippedNoChangesTotal.Add(1, map[string]string{})
		return true
	}
	c.lastConsolidationState = currentState
	return false
}

// boundAndCleanup limits the ranked nodes to maxNodesPerCycle and cleans up annotations
// on nodes that dropped out of the active set since the last reconcile.
func (c *Controller) boundAndCleanup(ctx context.Context, nodeRanks []NodeRank) []NodeRank {
	activeRanks := nodeRanks
	if len(activeRanks) > maxNodesPerCycle {
		activeRanks = activeRanks[:maxNodesPerCycle]
	}

	currentNodes := make(map[string]bool, len(activeRanks))
	for _, nr := range activeRanks {
		currentNodes[nr.Node.Name()] = true
	}

	for nodeName := range c.previouslyLabeledNodes {
		if !currentNodes[nodeName] {
			if err := c.annotationMgr.CleanupNodeAnnotations(ctx, nodeName); err != nil {
				log.FromContext(ctx).WithValues("node", nodeName).Error(err, "failed to clean up annotations on dropped node")
			}
		}
	}

	c.previouslyLabeledNodes = currentNodes
	return activeRanks
}
