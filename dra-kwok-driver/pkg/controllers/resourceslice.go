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

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/config"
)

// ResourceSliceController manages ResourceSlice lifecycle based on periodic polling of nodes and driver configuration
type ResourceSliceController struct {
	kubeClient  client.Client
	driverName  string
	configStore *config.Store
}

// NewResourceSliceController creates a new ResourceSlice controller
func NewResourceSliceController(kubeClient client.Client, driverName string, configStore *config.Store) *ResourceSliceController {
	return &ResourceSliceController{
		kubeClient:  kubeClient,
		driverName:  driverName,
		configStore: configStore,
	}
}

// Register registers the controller with the manager and starts the polling loop
func (r *ResourceSliceController) Register(ctx context.Context, mgr manager.Manager) error {
	// Start a goroutine for periodic reconciliation
	// This runs outside of controller-runtime's reconciliation loop
	go r.startPollingLoop(ctx)
	return nil
}

// startPollingLoop runs the reconciliation loop every 30 seconds
func (r *ResourceSliceController) startPollingLoop(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("resourceslice-poller")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Run immediately on startup
	logger.Info("starting initial reconciliation")
	if err := r.reconcileAllNodes(ctx); err != nil {
		logger.Error(err, "error during initial reconciliation")
	}

	// Then run periodically
	for {
		select {
		case <-ticker.C:
			logger.V(1).Info("starting periodic reconciliation")
			if err := r.reconcileAllNodes(ctx); err != nil {
				logger.Error(err, "error during periodic reconciliation")
			}
		case <-ctx.Done():
			logger.Info("stopping polling loop")
			return
		}
	}
}

// reconcileAllNodes reconciles ResourceSlices for all KWOK nodes in the cluster
func (r *ResourceSliceController) reconcileAllNodes(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("resourceslice")

	// Get current driver configuration
	cfg := r.configStore.Get()
	if cfg == nil {
		logger.V(1).Info("no driver configuration available, checking for orphaned ResourceSlices")
		// Even without config, we should clean up any orphaned ResourceSlices
		return r.cleanupOrphanedResourceSlices(ctx)
	}

	logger.V(1).Info("starting reconciliation cycle", "driver", cfg.Driver)

	// List all nodes in the cluster
	nodes := &corev1.NodeList{}
	if err := r.kubeClient.List(ctx, nodes); err != nil {
		return serrors.Wrap(fmt.Errorf("listing nodes, %w", err), "NodeList")
	}

	// Track which ResourceSlices should exist
	expectedResourceSlices := make(map[string]bool)

	// Process each node
	kwokNodeCount := 0
	errorCount := 0
	for _, node := range nodes.Items {
		if !r.isKWOKNode(&node) {
			continue
		}
		kwokNodeCount++

		// Process this node and track expected ResourceSlices
		expected, err := r.reconcileNodeResourceSlices(ctx, &node, cfg)
		if err != nil {
			logger.Error(err, "failed to reconcile node", "node", node.Name)
			errorCount++
			// Continue with other nodes as per design decision
			continue
		}

		// Add to expected set
		for _, name := range expected {
			expectedResourceSlices[name] = true
		}
	}

	// Clean up any ResourceSlices that shouldn't exist
	if err := r.cleanupUnexpectedResourceSlices(ctx, expectedResourceSlices); err != nil {
		logger.Error(err, "failed to cleanup unexpected ResourceSlices")
		errorCount++
	}

	logger.Info("completed reconciliation cycle",
		"kwok_nodes", kwokNodeCount,
		"errors", errorCount,
		"expected_slices", len(expectedResourceSlices),
	)

	if errorCount > 0 {
		return fmt.Errorf("reconciliation completed with %d errors", errorCount)
	}
	return nil
}

// reconcileNodeResourceSlices processes a single node and returns the names of ResourceSlices that should exist
func (r *ResourceSliceController) reconcileNodeResourceSlices(ctx context.Context, node *corev1.Node, cfg *config.Config) ([]string, error) {
	logger := log.FromContext(ctx).WithName("resourceslice").WithValues("node", node.Name)

	// Find all matching mappings for this node
	matchingMappings := r.findMatchingMappings(node, cfg.Mappings)

	if len(matchingMappings) == 0 {
		logger.V(1).Info("no matching mappings found for node")
		// No ResourceSlices should exist for this node
		return nil, nil
	}

	logger.V(1).Info("found matching mappings", "count", len(matchingMappings))

	// Batch operations for this node
	var expectedNames []string
	var createSlices []*resourcev1.ResourceSlice
	var updateSlices []*resourcev1.ResourceSlice

	// Process each mapping
	for _, mapping := range matchingMappings {
		resourceSliceName := fmt.Sprintf("%s-devices-%s", node.Name, mapping.Name)
		expectedNames = append(expectedNames, resourceSliceName)

		// Create the desired ResourceSlice
		desired := &resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceSliceName,
				Labels: map[string]string{
					"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
					"kwok.x-k8s.io/node":       node.Name,
					"kwok.x-k8s.io/mapping":    mapping.Name,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       node.Name,
						UID:        node.UID,
					},
				},
			},
			Spec: mapping.ResourceSlice,
		}

		// Override node-specific fields
		desired.Spec.NodeName = &node.Name
		desired.Spec.Driver = r.driverName

		// Check if it exists
		existing := &resourcev1.ResourceSlice{}
		if err := r.kubeClient.Get(ctx, types.NamespacedName{Name: resourceSliceName}, existing); err != nil {
			if errors.IsNotFound(err) {
				createSlices = append(createSlices, desired)
			} else {
				return nil, serrors.Wrap(fmt.Errorf("getting resourceslice, %w", err), "ResourceSlice", klog.KRef("", resourceSliceName))
			}
		} else {
			// Check if update is needed
			if !r.resourceSliceEqual(existing, desired) {
				existing.Spec = desired.Spec
				existing.Labels = desired.Labels
				updateSlices = append(updateSlices, existing)
			}
		}
	}

	// Apply batched creates
	for _, slice := range createSlices {
		logger.Info("creating resourceslice",
			"resourceslice", slice.Name,
			"mapping", slice.Labels["kwok.x-k8s.io/mapping"],
			"devices", len(slice.Spec.Devices),
		)
		if err := r.kubeClient.Create(ctx, slice); err != nil {
			return nil, serrors.Wrap(fmt.Errorf("creating resourceslice, %w", err), "ResourceSlice", klog.KRef("", slice.Name))
		}
	}

	// Apply batched updates
	for _, slice := range updateSlices {
		logger.Info("updating resourceslice",
			"resourceslice", slice.Name,
			"mapping", slice.Labels["kwok.x-k8s.io/mapping"],
			"devices", len(slice.Spec.Devices),
		)
		if err := r.kubeClient.Update(ctx, slice); err != nil {
			return nil, serrors.Wrap(fmt.Errorf("updating resourceslice, %w", err), "ResourceSlice", klog.KRef("", slice.Name))
		}
	}

	return expectedNames, nil
}

// resourceSliceEqual checks if two ResourceSlices have the same spec and labels
func (r *ResourceSliceController) resourceSliceEqual(a, b *resourcev1.ResourceSlice) bool {
	// Simple comparison - in production you might want a deeper comparison
	// For now, we always update to ensure consistency
	return false
}

// cleanupUnexpectedResourceSlices removes ResourceSlices that shouldn't exist
func (r *ResourceSliceController) cleanupUnexpectedResourceSlices(ctx context.Context, expectedSlices map[string]bool) error {
	logger := log.FromContext(ctx).WithName("resourceslice")

	// List all ResourceSlices managed by this driver
	resourceSlices := &resourcev1.ResourceSliceList{}
	if err := r.kubeClient.List(ctx, resourceSlices, client.MatchingLabels{
		"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
	}); err != nil {
		return serrors.Wrap(fmt.Errorf("listing resourceslices for cleanup, %w", err))
	}

	// Delete unexpected ones
	deletedCount := 0
	for _, rs := range resourceSlices.Items {
		if !expectedSlices[rs.Name] {
			logger.Info("deleting unexpected resourceslice", "resourceslice", rs.Name)
			if err := r.kubeClient.Delete(ctx, &rs); err != nil && !errors.IsNotFound(err) {
				// Log error but continue with other deletions
				logger.Error(err, "failed to delete resourceslice", "resourceslice", rs.Name)
			} else {
				deletedCount++
			}
		}
	}

	if deletedCount > 0 {
		logger.Info("cleaned up unexpected resourceslices", "count", deletedCount)
	}

	return nil
}

// cleanupOrphanedResourceSlices removes all ResourceSlices when no configuration exists
func (r *ResourceSliceController) cleanupOrphanedResourceSlices(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("resourceslice")

	// List all ResourceSlices managed by this driver
	resourceSlices := &resourcev1.ResourceSliceList{}
	if err := r.kubeClient.List(ctx, resourceSlices, client.MatchingLabels{
		"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
	}); err != nil {
		return serrors.Wrap(fmt.Errorf("listing resourceslices for cleanup, %w", err))
	}

	if len(resourceSlices.Items) == 0 {
		return nil
	}

	// Delete all since we have no configuration
	logger.Info("no configuration exists, cleaning up all ResourceSlices", "count", len(resourceSlices.Items))
	for _, rs := range resourceSlices.Items {
		if err := r.kubeClient.Delete(ctx, &rs); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "failed to delete orphaned resourceslice", "resourceslice", rs.Name)
			// Continue with other deletions
		}
	}

	return nil
}

// isKWOKNode determines if a node is a KWOK node created by Karpenter's KWOK provider
func (r *ResourceSliceController) isKWOKNode(node *corev1.Node) bool {
	// Karpenter KWOK provider adds this annotation to all nodes it creates
	_, ok := node.Annotations["kwok.x-k8s.io/node"]
	return ok
}

// findMatchingMappings finds all mappings that match the node's labels
func (r *ResourceSliceController) findMatchingMappings(node *corev1.Node, mappings []config.Mapping) []config.Mapping {
	var matches []config.Mapping
	for _, mapping := range mappings {
		if r.nodeMatchesTerms(node, mapping.NodeSelectorTerms) {
			matches = append(matches, mapping)
		}
	}
	return matches
}

// nodeMatchesTerms returns true if the node matches any of the NodeSelectorTerms (terms are ORed together)
func (r *ResourceSliceController) nodeMatchesTerms(node *corev1.Node, terms []corev1.NodeSelectorTerm) bool {
	nodeLabels := labels.Set(node.Labels)

	for _, term := range terms {
		if r.nodeMatchesTerm(nodeLabels, term) {
			return true // Any term matching is sufficient (OR logic)
		}
	}
	return false
}

// nodeMatchesTerm returns true if the node matches a single NodeSelectorTerm
func (r *ResourceSliceController) nodeMatchesTerm(nodeLabels labels.Set, term corev1.NodeSelectorTerm) bool {
	// MatchExpressions must all match (AND logic within a term)
	for _, expr := range term.MatchExpressions {
		req, err := labels.NewRequirement(expr.Key, selection.Operator(expr.Operator), expr.Values)
		if err != nil {
			return false // Invalid expression doesn't match
		}
		if !req.Matches(nodeLabels) {
			return false // All expressions must match
		}
	}

	// MatchFields are not commonly used in this context, but we should handle them
	// For KWOK nodes, we typically only use MatchExpressions
	if len(term.MatchFields) > 0 {
		// MatchFields require access to the full Node object, not just labels
		// For now, we'll skip this since KWOK nodes don't typically use field selectors
		return false
	}

	return true // All match expressions passed
}

// GetResourceSlicesForNode returns all ResourceSlices managed by this driver for a specific node
func (r *ResourceSliceController) GetResourceSlicesForNode(ctx context.Context, nodeName string) ([]*resourcev1.ResourceSlice, error) {
	resourceSlices := &resourcev1.ResourceSliceList{}
	if err := r.kubeClient.List(ctx, resourceSlices, client.MatchingLabels{
		"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
		"kwok.x-k8s.io/node":       nodeName,
	}); err != nil {
		return nil, err
	}

	result := make([]*resourcev1.ResourceSlice, len(resourceSlices.Items))
	for i := range resourceSlices.Items {
		result[i] = &resourceSlices.Items[i]
	}
	return result, nil
}
