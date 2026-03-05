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

const reconcileInterval = time.Minute

// Controller manages pod deletion cost annotations for Karpenter-managed nodes
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster
	recorder      events.Recorder

	rankingEngine  *RankingEngine
	annotationMgr  *AnnotationManager
	changeDetector *ChangeDetector
}

// NewController creates a new pod deletion cost controller
func NewController(
	clk clock.Clock,
	kubeClient client.Client,
	cloudProvider cloudprovider.CloudProvider,
	cluster *state.Cluster,
	recorder events.Recorder,
) *Controller {
	// Get options from context to determine ranking strategy
	// This will be set during reconcile when we have the context
	return &Controller{
		clock:          clk,
		kubeClient:     kubeClient,
		cloudProvider:  cloudProvider,
		cluster:        cluster,
		recorder:       recorder,
		annotationMgr:  NewAnnotationManager(kubeClient, recorder),
		changeDetector: NewChangeDetector(),
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

	// Get options from context
	opts := options.FromContext(ctx)

	// Check if feature is enabled via feature gate
	if !opts.FeatureGates.PodDeletionCostManagement {
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	// Initialize ranking engine with configured strategy if not already done
	if c.rankingEngine == nil {
		c.rankingEngine = NewRankingEngine(RankingStrategy(opts.PodDeletionCostRankingStrategy))
	}

	// Get all Karpenter-managed nodes from cluster state
	var nodes []*state.StateNode
	for node := range c.cluster.Nodes() {
		nodes = append(nodes, node)
	}

	// If no nodes, nothing to do
	if len(nodes) == 0 {
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	// Check change detection if enabled
	if opts.PodDeletionCostChangeDetection {
		changed, err := c.changeDetector.HasChanged(ctx, c.kubeClient, nodes)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to check for changes")
			// Continue with ranking even if change detection fails
		} else if !changed {
			// No changes detected, skip ranking
			log.FromContext(ctx).V(1).Info("no changes detected, skipping pod deletion cost update")
			SkippedNoChangesTotal.Add(1, map[string]string{})
			return reconciler.Result{RequeueAfter: reconcileInterval}, nil
		}
	}

	// Call ranking engine to rank nodes
	nodeRanks, err := c.rankingEngine.RankNodes(ctx, c.kubeClient, nodes)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to rank nodes")
		// Publish disabled event for fatal ranking errors
		c.recorder.Publish(DisabledEvent(fmt.Sprintf("failed to rank nodes: %v", err)))
		return reconciler.Result{RequeueAfter: reconcileInterval}, err
	}

	// Publish ranking completed event
	c.recorder.Publish(RankingCompletedEvent(len(nodeRanks), string(c.rankingEngine.strategy)))

	// Call annotation manager to update pods
	if err := c.annotationMgr.UpdatePodDeletionCosts(ctx, nodeRanks); err != nil {
		log.FromContext(ctx).Error(err, "failed to update pod deletion costs")
		// Publish disabled event for fatal annotation errors
		c.recorder.Publish(DisabledEvent(fmt.Sprintf("failed to update pod deletion costs: %v", err)))
		return reconciler.Result{RequeueAfter: reconcileInterval}, err
	}

	log.FromContext(ctx).V(1).WithValues("nodeCount", len(nodeRanks)).Info("updated pod deletion costs")

	return reconciler.Result{RequeueAfter: reconcileInterval}, nil
}
