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
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
)

// protectedNodeEntry represents a protected node with its expiration time.
type protectedNodeEntry struct {
	expiration time.Time
}

// protectedNodes tracks nodes that meet the criteria for consolidationGracePeriod protection:
// - High utilization (>= threshold)
// - Stable (no pod activity for consolidateAfter duration)
// The map key is the node's providerID.
type protectedNodes struct {
	mu    sync.RWMutex
	nodes map[string]protectedNodeEntry
}

// Controller is an independent observer that tracks well-utilized, stable nodes
// for nodepools configured with consolidationGracePeriod.
//
// Protection Logic:
// When a node becomes consolidatable (passes consolidateAfter) AND has utilization >= threshold,
// it is protected from consolidation for consolidationGracePeriod duration.
//
// This breaks the consolidation cycle where:
// 1. Old stable nodes become consolidation targets
// 2. Pods move to newer nodes
// 3. Newer nodes reset their consolidateAfter timer
// 4. Cycle repeats
//
// By protecting high-utilization stable nodes, we give them time to:
// - Continue serving their current workloads
// - Receive new pods (from other consolidations)
// - Reset their consolidateAfter timer naturally
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster
	protected     *protectedNodes
}

// NewController constructs a consolidation observer controller
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster) *Controller {
	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		cluster:       cluster,
		protected: &protectedNodes{
			nodes: make(map[string]protectedNodeEntry),
		},
	}
}

// IsProtected returns true if the node with the given providerID is currently
// protected from consolidation based on consolidationGracePeriod configuration.
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

// calculateNodeUtilization calculates the overall resource utilization percentage for a node.
// Returns the maximum utilization across CPU and memory.
func (c *Controller) calculateNodeUtilization(ctx context.Context, node *corev1.Node) (float64, error) {
	// Get the state node from cluster state
	clusterNodes := c.cluster.DeepCopyNodes()
	var stateNode *state.StateNode
	for _, sn := range clusterNodes {
		if sn.ProviderID() == node.Spec.ProviderID {
			stateNode = sn
			break
		}
	}

	if stateNode == nil {
		// If not in cluster state, calculate from node directly
		allocatable := node.Status.Allocatable
		// Get pod requests from the node
		var pods corev1.PodList
		if err := c.kubeClient.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
			return 0, fmt.Errorf("listing pods, %w", err)
		}

		totalRequests := make(corev1.ResourceList)
		for i := range pods.Items {
			pod := &pods.Items[i]
			if podutils.IsTerminal(pod) {
				continue
			}
			for _, container := range pod.Spec.Containers {
				for resName, quantity := range container.Resources.Requests {
					if existing, ok := totalRequests[resName]; ok {
						existing.Add(quantity)
						totalRequests[resName] = existing
					} else {
						totalRequests[resName] = quantity.DeepCopy()
					}
				}
			}
		}

		// Calculate utilization for CPU and memory
		cpuUtil := 0.0
		memUtil := 0.0

		if cpuAlloc := allocatable[corev1.ResourceCPU]; !cpuAlloc.IsZero() {
			if cpuReq, ok := totalRequests[corev1.ResourceCPU]; ok {
				cpuUtil = float64(cpuReq.MilliValue()) / float64(cpuAlloc.MilliValue())
			}
		}

		if memAlloc := allocatable[corev1.ResourceMemory]; !memAlloc.IsZero() {
			if memReq, ok := totalRequests[corev1.ResourceMemory]; ok {
				memUtil = float64(memReq.Value()) / float64(memAlloc.Value())
			}
		}

		// Return maximum utilization
		return lo.Max([]float64{cpuUtil, memUtil}) * 100, nil
	}

	// Use state node for more accurate calculation
	allocatable := stateNode.Allocatable()
	podRequests := stateNode.PodRequests()

	cpuUtil := 0.0
	memUtil := 0.0

	if cpuAlloc, ok := allocatable[corev1.ResourceCPU]; ok && !cpuAlloc.IsZero() {
		if cpuReq, ok := podRequests[corev1.ResourceCPU]; ok {
			cpuUtil = float64(cpuReq.MilliValue()) / float64(cpuAlloc.MilliValue())
		}
	}

	if memAlloc, ok := allocatable[corev1.ResourceMemory]; ok && !memAlloc.IsZero() {
		if memReq, ok := podRequests[corev1.ResourceMemory]; ok {
			memUtil = float64(memReq.Value()) / float64(memAlloc.Value())
		}
	}

	// Return maximum utilization as percentage
	return lo.Max([]float64{cpuUtil, memUtil}) * 100, nil
}

// Reconcile evaluates whether a node should be protected from consolidation.
//
// Protection Criteria (ALL must be true):
// 1. consolidationGracePeriod is configured on the NodePool
// 2. Node has passed consolidateAfter (stable - no pod events for consolidateAfter duration)
// 3. Node utilization >= consolidationGracePeriodUtilizationThreshold (configurable, default 50%)
//
// When protected, the node will not be considered for consolidation for consolidationGracePeriod duration.
func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	log.FromContext(ctx).Info("consolidationGracePeriod: observer reconciling", "nodeClaim", nodeClaim.Name)
	
	if !nodeclaimutils.IsManaged(nodeClaim, c.cloudProvider) {
		log.FromContext(ctx).Info("consolidationGracePeriod: nodeClaim not managed by this cloud provider, skipping")
		return reconcile.Result{}, nil
	}
	log.FromContext(ctx).Info("consolidationGracePeriod: nodeClaim is managed, continuing")

	// Get the NodePool for this NodeClaim
	nodePoolName := nodeClaim.Labels[v1.NodePoolLabelKey]
	if nodePoolName == "" {
		log.FromContext(ctx).Info("consolidationGracePeriod: no NodePool label, skipping")
		return reconcile.Result{}, nil
	}

	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(fmt.Errorf("getting nodepool, %w", err))
	}

	// Check if consolidationGracePeriod is configured for this NodePool
	if nodePool.Spec.Disruption.ConsolidationGracePeriod.Duration == nil {
		log.FromContext(ctx).Info("consolidationGracePeriod: not configured for NodePool, skipping", "nodePool", nodePoolName)
		return reconcile.Result{}, nil
	}

	// Check if consolidateAfter is configured (required for this feature)
	if nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		log.FromContext(ctx).Info("consolidationGracePeriod: consolidateAfter not configured, skipping", "nodePool", nodePoolName)
		return reconcile.Result{}, nil
	}
	
	log.FromContext(ctx).Info("consolidationGracePeriod: feature configured, processing", 
		"nodePool", nodePoolName,
		"consolidationGracePeriod", nodePool.Spec.Disruption.ConsolidationGracePeriod.Duration.String(),
		"consolidateAfter", nodePool.Spec.Disruption.ConsolidateAfter.Duration.String())

	// Get the providerID from NodeClaim status
	providerID := nodeClaim.Status.ProviderID
	if providerID == "" {
		// If providerID is not set yet, wait for it to be populated
		return reconcile.Result{}, nil
	}

	// Get the node for utilization calculation
	node, err := nodeclaimutils.NodeForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		// If node doesn't exist yet, that's okay - we'll retry later
		return reconcile.Result{}, client.IgnoreNotFound(nil)
	}

	// Check if node has been initialized
	initialized := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
	if !initialized.IsTrue() {
		return reconcile.Result{}, nil
	}

	// Get configuration values
	consolidateAfterDuration := lo.FromPtr(nodePool.Spec.Disruption.ConsolidateAfter.Duration)
	consolidationGracePeriodDuration := lo.FromPtr(nodePool.Spec.Disruption.ConsolidationGracePeriod.Duration)

	// Get utilization threshold (default to 50 if not set)
	utilizationThreshold := float64(50)
	if nodePool.Spec.Disruption.ConsolidationGracePeriodUtilizationThreshold != nil {
		utilizationThreshold = float64(*nodePool.Spec.Disruption.ConsolidationGracePeriodUtilizationThreshold)
	}

	// Determine the time to check for stability
	// Use LastPodEventTime if available, otherwise use initialization time
	timeToCheck := lo.Ternary(!nodeClaim.Status.LastPodEventTime.IsZero(),
		nodeClaim.Status.LastPodEventTime.Time,
		initialized.LastTransitionTime.Time)

	timeSinceLastPodEvent := c.clock.Since(timeToCheck)

	// Check if node has passed consolidateAfter (is stable)
	// If not stable yet, the existing consolidateAfter logic already protects it
	if timeSinceLastPodEvent < consolidateAfterDuration {
		// Node is not yet consolidatable - remove from protected list if present
		c.protected.mu.Lock()
		delete(c.protected.nodes, providerID)
		c.protected.mu.Unlock()
		// Requeue to check again when the node becomes consolidatable
		// This ensures we can protect high-utilization nodes as soon as they become consolidation candidates
		consolidatableTime := timeToCheck.Add(consolidateAfterDuration)
		return reconcile.Result{RequeueAfter: consolidatableTime.Sub(c.clock.Now())}, nil
	}

	// Node has passed consolidateAfter - it's now a consolidation candidate
	// Check if it qualifies for consolidationGracePeriod protection

	// Calculate node utilization
	utilization, err := c.calculateNodeUtilization(ctx, node)
	if err != nil {
		log.FromContext(ctx).V(1).Error(err, "failed to calculate node utilization")
		return reconcile.Result{}, nil
	}

	// Check if utilization is below threshold
	if utilization < utilizationThreshold {
		// Low utilization - remove protection and allow consolidation
		// This is cost-efficient: underutilized nodes should be consolidated
		c.protected.mu.Lock()
		delete(c.protected.nodes, providerID)
		c.protected.mu.Unlock()

		log.FromContext(ctx).Info("consolidationGracePeriod: node below utilization threshold, allowing consolidation",
			"providerID", providerID,
			"utilization", utilization,
			"threshold", utilizationThreshold)

		return reconcile.Result{}, nil
	}

	// HIGH UTILIZATION + STABLE = PROTECT
	// This is a productive node that shouldn't be disrupted
	// Calculate expiration: from when the node became consolidatable + consolidationGracePeriod
	consolidatableTime := timeToCheck.Add(consolidateAfterDuration)
	expiration := consolidatableTime.Add(consolidationGracePeriodDuration)

	// Add or update the node in the protected list
	c.protected.mu.Lock()
	c.protected.nodes[providerID] = protectedNodeEntry{
		expiration: expiration,
	}
	c.protected.mu.Unlock()

	log.FromContext(ctx).Info("consolidationGracePeriod: protecting high-utilization stable node",
		"providerID", providerID,
		"utilization", utilization,
		"threshold", utilizationThreshold,
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
