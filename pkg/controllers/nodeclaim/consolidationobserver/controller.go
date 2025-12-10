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

package consolidationobserver

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/samber/lo"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// protectedNodeEntry represents a protected node with its expiration time.
type protectedNodeEntry struct {
	expiration time.Time
}

// protectedNodes tracks nodes that are protected from re-evaluation for consolidation.
// Protection is granted to stable nodes (passed consolidateAfter) to prevent consolidation churn.
// The map key is the node's providerID.
type protectedNodes struct {
	mu    sync.RWMutex
	nodes map[string]protectedNodeEntry
}

// Controller is an independent observer that tracks stable nodes
// for nodepools configured with consolidationGracePeriod.
//
// Protection Logic:
// When a node becomes consolidatable (passes consolidateAfter), it is protected
// from re-evaluation for consolidationGracePeriod duration.
//
// This aligns with Karpenter's cost-based optimization:
// - If consolidation can find a cheaper configuration, it will act during the initial evaluation
// - The grace period prevents repeated re-evaluation of nodes that are already cost-optimal
// - This breaks the consolidation churn cycle without conflicting with cost optimization
//
// The key insight is that we're not adding a utilization gate (which would conflict with
// cost optimization). Instead, we're adding a cooldown to prevent repeated evaluation
// of the same stable nodes.
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	protected     *protectedNodes
}

// NewController constructs a consolidation observer controller
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		protected: &protectedNodes{
			nodes: make(map[string]protectedNodeEntry),
		},
	}
}

// IsProtected returns true if the node with the given providerID is currently
// protected from re-evaluation based on consolidationGracePeriod configuration.
func (c *Controller) IsProtected(providerID string) bool {
	c.protected.mu.RLock()
	defer c.protected.mu.RUnlock()

	entry, exists := c.protected.nodes[providerID]
	if !exists {
		return false
	}

	// Check if protection has expired
	if c.clock.Now().After(entry.expiration) {
		return false
	}

	return true
}

// GetProtectionExpiration returns the expiration time for a protected node,
// or zero time if not protected.
func (c *Controller) GetProtectionExpiration(providerID string) time.Time {
	c.protected.mu.RLock()
	defer c.protected.mu.RUnlock()

	entry, exists := c.protected.nodes[providerID]
	if !exists || c.clock.Now().After(entry.expiration) {
		return time.Time{}
	}
	return entry.expiration
}

// Reconcile evaluates whether a node should be protected from re-evaluation.
//
// Protection Criteria (ALL must be true):
// 1. consolidationGracePeriod is configured on the NodePool
// 2. Node has passed consolidateAfter (stable - no pod events for consolidateAfter duration)
//
// When protected, the node will not be re-evaluated for consolidation for
// consolidationGracePeriod duration. This works alongside Karpenter's cost-based
// consolidation - if a cheaper configuration exists, consolidation will act during
// the initial evaluation window.
func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	log.FromContext(ctx).V(1).Info("consolidationGracePeriod: observer reconciling", "nodeClaim", nodeClaim.Name)

	if !nodeclaimutils.IsManaged(nodeClaim, c.cloudProvider) {
		return reconcile.Result{}, nil
	}

	// Get the NodePool for this NodeClaim
	nodePoolName := nodeClaim.Labels[v1.NodePoolLabelKey]
	if nodePoolName == "" {
		return reconcile.Result{}, nil
	}

	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(fmt.Errorf("getting nodepool, %w", err))
	}

	// Check if consolidationGracePeriod is configured for this NodePool
	if nodePool.Spec.Disruption.ConsolidationGracePeriod.Duration == nil {
		return reconcile.Result{}, nil
	}

	// Check if consolidateAfter is configured (required for this feature)
	if nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		return reconcile.Result{}, nil
	}

	// Get the providerID from NodeClaim status
	providerID := nodeClaim.Status.ProviderID
	if providerID == "" {
		// If providerID is not set yet, wait for it to be populated
		return reconcile.Result{}, nil
	}

	// Check if node has been initialized
	initialized := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
	if !initialized.IsTrue() {
		return reconcile.Result{}, nil
	}

	// Get configuration values
	consolidateAfterDuration := lo.FromPtr(nodePool.Spec.Disruption.ConsolidateAfter.Duration)
	consolidationGracePeriodDuration := lo.FromPtr(nodePool.Spec.Disruption.ConsolidationGracePeriod.Duration)

	// Determine the time to check for stability
	// Use LastPodEventTime if available, otherwise use initialization time
	timeToCheck := lo.Ternary(!nodeClaim.Status.LastPodEventTime.IsZero(),
		nodeClaim.Status.LastPodEventTime.Time,
		initialized.LastTransitionTime.Time)

	timeSinceLastPodEvent := c.clock.Since(timeToCheck)

	// Check if node has passed consolidateAfter (is stable)
	// If not stable yet, the existing consolidateAfter logic handles it
	if timeSinceLastPodEvent < consolidateAfterDuration {
		// Node is not yet consolidatable - remove from protected list if present
		c.protected.mu.Lock()
		delete(c.protected.nodes, providerID)
		c.protected.mu.Unlock()
		// Requeue to check again when the node becomes consolidatable
		consolidatableTime := timeToCheck.Add(consolidateAfterDuration)
		return reconcile.Result{RequeueAfter: consolidatableTime.Sub(c.clock.Now())}, nil
	}

	// Node has passed consolidateAfter - it's now a consolidation candidate
	// Protect it from repeated re-evaluation for consolidationGracePeriod duration
	//
	// Key insight: This doesn't conflict with cost optimization because:
	// 1. During the consolidateAfter window, consolidation has already evaluated this node
	// 2. If a cheaper configuration exists, consolidation would have acted
	// 3. The grace period just prevents the same node from being re-evaluated repeatedly
	// 4. If pod events occur, the node becomes non-consolidatable and protection is removed

	// Calculate expiration: from when the node became consolidatable + consolidationGracePeriod
	consolidatableTime := timeToCheck.Add(consolidateAfterDuration)
	expiration := consolidatableTime.Add(consolidationGracePeriodDuration)

	// Add or update the node in the protected list
	c.protected.mu.Lock()
	c.protected.nodes[providerID] = protectedNodeEntry{
		expiration: expiration,
	}
	c.protected.mu.Unlock()

	log.FromContext(ctx).Info("consolidationGracePeriod: protecting stable node from re-evaluation",
		"providerID", providerID,
		"timeSinceLastPodEvent", timeSinceLastPodEvent.String(),
		"consolidatableAt", consolidatableTime,
		"protectedUntil", expiration)

	// Requeue to re-evaluate when protection expires
	requeueAfter := expiration.Sub(c.clock.Now())
	if requeueAfter > 0 {
		return reconcile.Result{RequeueAfter: requeueAfter}, nil
	}

	return reconcile.Result{}, nil
}

// CleanupExpiredNodes removes nodes from the protected list that have expired.
// This should be called periodically to prevent memory leaks.
func (c *Controller) CleanupExpiredNodes() {
	c.protected.mu.Lock()
	defer c.protected.mu.Unlock()

	now := c.clock.Now()
	for providerID, entry := range c.protected.nodes {
		if now.After(entry.expiration) {
			delete(c.protected.nodes, providerID)
		}
	}
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	// Start a background goroutine to periodically clean up expired nodes
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.CleanupExpiredNodes()
			}
		}
	}()

	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclaim.consolidationobserver").
		For(&v1.NodeClaim{}).
		Watches(&v1.NodePool{}, nodeclaimutils.NodePoolEventHandler(c.kubeClient, c.cloudProvider)).
		Watches(&corev1.Pod{}, nodeclaimutils.PodEventHandler(c.kubeClient, c.cloudProvider)).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
