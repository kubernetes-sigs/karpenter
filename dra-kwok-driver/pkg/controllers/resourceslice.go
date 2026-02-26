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
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/go-logr/logr"
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

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/apis/v1alpha1"
)

// ResourceSliceController manages ResourceSlice lifecycle based on periodic polling of nodes and DRAConfig CRD
type ResourceSliceController struct {
	kubeClient client.Client
	namespace  string // Namespace where DRAConfig CRD is located
}

// NewResourceSliceController creates a new ResourceSlice controller
func NewResourceSliceController(kubeClient client.Client, namespace string) *ResourceSliceController {
	return &ResourceSliceController{
		kubeClient: kubeClient,
		namespace:  namespace,
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

	// List all DRAConfig CRDs
	draConfigs := &v1alpha1.DRAConfigList{}
	err := r.kubeClient.List(ctx, draConfigs, &client.ListOptions{
		Namespace: r.namespace,
	})
	if err != nil {
		return fmt.Errorf("listing DRAConfigs: %w", err)
	}

	if len(draConfigs.Items) == 0 {
		logger.V(1).Info("no DRAConfigs found, cleaning up all ResourceSlices")
		return r.cleanupOrphanedResourceSlices(ctx)
	}

	// Group configs by driver name
	configsByDriver := r.groupConfigsByDriver(logger, draConfigs.Items)

	logger.Info("discovered drivers from DRAConfigs",
		"driver_count", len(configsByDriver),
		"config_count", len(draConfigs.Items))

	// List all nodes once
	nodes := &corev1.NodeList{}
	if err := r.kubeClient.List(ctx, nodes); err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	// Track expected ResourceSlices across all drivers
	expectedResourceSlices := make(map[string]bool)
	errorCount := 0

	// Process each driver independently
	for driverName, cfg := range configsByDriver {
		expected, errs := r.processDriver(ctx, logger, driverName, cfg, nodes.Items)
		errorCount += errs
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
		"drivers", len(configsByDriver),
		"errors", errorCount,
		"expected_slices", len(expectedResourceSlices),
	)

	if errorCount > 0 {
		return fmt.Errorf("reconciliation completed with %d errors", errorCount)
	}
	return nil
}

// groupConfigsByDriver groups DRAConfigs by their driver name, warning about duplicates
func (r *ResourceSliceController) groupConfigsByDriver(logger logr.Logger, configs []v1alpha1.DRAConfig) map[string]v1alpha1.DRAConfig {
	configsByDriver := make(map[string]v1alpha1.DRAConfig)
	for _, cfg := range configs {
		driverName := cfg.Spec.Driver
		if existingCfg, exists := configsByDriver[driverName]; exists {
			// Duplicate driver! Log warning and skip
			logger.Info("WARNING: Multiple DRAConfigs found for same driver, using first one",
				"driver", driverName,
				"first_config", existingCfg.Name,
				"duplicate_config", cfg.Name)
			continue
		}
		configsByDriver[driverName] = cfg
	}
	return configsByDriver
}

// processDriver processes all nodes for a single driver, returning expected ResourceSlice names and error count
func (r *ResourceSliceController) processDriver(
	ctx context.Context,
	logger logr.Logger,
	driverName string,
	cfg v1alpha1.DRAConfig,
	nodes []corev1.Node,
) ([]string, int) {
	logger.V(1).Info("reconciling driver", "driver", driverName, "config", cfg.Name)

	var expectedNames []string
	errorCount := 0
	kwokNodeCount := 0

	// Use mappings directly from this driver's config
	mappings := cfg.Spec.Mappings

	// Process each node for this driver
	for i := range nodes {
		node := &nodes[i]
		if !r.isKWOKNode(node) {
			continue
		}
		kwokNodeCount++

		// Reconcile this node with mappings from this driver's config
		expected, err := r.reconcileNodeResourceSlicesForDriver(ctx, node, driverName, mappings)
		if err != nil {
			logger.Error(err, "failed to reconcile node", "node", node.Name, "driver", driverName)
			errorCount++
			continue
		}

		expectedNames = append(expectedNames, expected...)
	}

	logger.Info("completed driver reconciliation",
		"driver", driverName,
		"config", cfg.Name,
		"kwok_nodes", kwokNodeCount,
		"mappings", len(mappings))

	return expectedNames, errorCount
}

// reconcileNodeResourceSlicesForDriver processes a single node for a specific driver
func (r *ResourceSliceController) reconcileNodeResourceSlicesForDriver(
	ctx context.Context,
	node *corev1.Node,
	driverName string,
	mappings []v1alpha1.Mapping,
) ([]string, error) {
	logger := log.FromContext(ctx).WithName("resourceslice").WithValues("node", node.Name, "driver", driverName)

	// Find all matching mappings for this node
	matchingMappings := r.findMatchingMappings(node, mappings)

	if len(matchingMappings) == 0 {
		logger.V(1).Info("no matching mappings found for node")
		return nil, nil
	}

	logger.V(1).Info("found matching mappings", "count", len(matchingMappings))

	var expectedNames []string

	// Process each mapping
	for _, mapping := range matchingMappings {
		// ResourceSlice naming: <driver-sanitized>-<nodename>-<mapping-name>
		driverSanitized := sanitizeDriverName(driverName)
		resourceSliceName := fmt.Sprintf("%s-%s-%s", driverSanitized, node.Name, mapping.Name)
		expectedNames = append(expectedNames, resourceSliceName)

		// Check if ResourceSlice exists
		existing := &resourcev1.ResourceSlice{}
		err := r.kubeClient.Get(ctx, types.NamespacedName{Name: resourceSliceName}, existing)

		if err == nil {
			// ResourceSlice exists - check if update is needed
			if !r.resourceSliceNeedsUpdate(existing, &mapping.ResourceSlice) {
				continue
			}

			// Update needed - bump generation and update in place
			oldGeneration := existing.Spec.Pool.Generation
			newGeneration := oldGeneration + 1

			existing.Spec = mapping.ResourceSlice.ToResourceSliceSpec(driverName)
			existing.Spec.NodeName = &node.Name
			existing.Spec.Pool.Generation = newGeneration

			logger.Info("updating resourceslice",
				"resourceslice", resourceSliceName,
				"old_generation", oldGeneration,
				"new_generation", newGeneration,
				"devices", len(existing.Spec.Devices),
			)

			if err := r.kubeClient.Update(ctx, existing); err != nil {
				return nil, serrors.Wrap(fmt.Errorf("updating resourceslice, %w", err), "ResourceSlice", klog.KRef("", resourceSliceName))
			}
		} else if errors.IsNotFound(err) {
			// ResourceSlice doesn't exist - create it
			desired := &resourcev1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name: resourceSliceName,
					Labels: map[string]string{
						"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
						"kwok.x-k8s.io/node":       node.Name,
						"kwok.x-k8s.io/mapping":    mapping.Name,
						"kwok.x-k8s.io/driver":     driverName,
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
				Spec: mapping.ResourceSlice.ToResourceSliceSpec(driverName),
			}

			desired.Spec.NodeName = &node.Name
			desired.Spec.Pool.Generation = 0

			logger.Info("creating resourceslice",
				"resourceslice", desired.Name,
				"mapping", desired.Labels["kwok.x-k8s.io/mapping"],
				"driver", driverName,
				"generation", 0,
				"devices", len(desired.Spec.Devices),
			)
			if err := r.kubeClient.Create(ctx, desired); err != nil {
				return nil, serrors.Wrap(fmt.Errorf("creating resourceslice, %w", err), "ResourceSlice", klog.KRef("", desired.Name))
			}
		} else {
			return nil, serrors.Wrap(fmt.Errorf("getting resourceslice, %w", err), "ResourceSlice", klog.KRef("", resourceSliceName))
		}
	}

	return expectedNames, nil
}

// isKWOKNode checks if a node is a KWOK node by looking for the Karpenter KWOK annotation
func (r *ResourceSliceController) isKWOKNode(node *corev1.Node) bool {
	if node.Annotations == nil {
		return false
	}
	// Check for kwok.x-k8s.io/node annotation (set by Karpenter when using KWOK provider)
	_, hasKWOKAnnotation := node.Annotations["kwok.x-k8s.io/node"]
	return hasKWOKAnnotation
}

// findMatchingMappings returns all mappings that match the given node
func (r *ResourceSliceController) findMatchingMappings(node *corev1.Node, mappings []v1alpha1.Mapping) []v1alpha1.Mapping {
	var matches []v1alpha1.Mapping

	for _, mapping := range mappings {
		if r.nodeSelectorMatches(node, mapping.NodeSelectorTerms) {
			matches = append(matches, mapping)
		}
	}

	return matches
}

// nodeSelectorMatches checks if a node matches ANY of the NodeSelectorTerms (OR logic)
func (r *ResourceSliceController) nodeSelectorMatches(node *corev1.Node, terms []corev1.NodeSelectorTerm) bool {
	// Empty terms means match nothing
	if len(terms) == 0 {
		return false
	}

	// OR across terms - node must match at least one term
	for _, term := range terms {
		if r.nodeMatchesTerm(node, term) {
			return true
		}
	}

	return false
}

// nodeMatchesTerm checks if a node matches ALL requirements in a NodeSelectorTerm (AND logic)
func (r *ResourceSliceController) nodeMatchesTerm(node *corev1.Node, term corev1.NodeSelectorTerm) bool {
	// AND across match expressions - node must match ALL expressions in the term
	for _, expr := range term.MatchExpressions {
		if !r.nodeMatchesExpression(node, expr) {
			return false
		}
	}

	// AND across match fields
	for _, field := range term.MatchFields {
		if !r.nodeMatchesFieldExpression(node, field) {
			return false
		}
	}

	return true
}

// nodeMatchesExpression checks if a node matches a single NodeSelectorRequirement
func (r *ResourceSliceController) nodeMatchesExpression(node *corev1.Node, expr corev1.NodeSelectorRequirement) bool {
	nodeLabels := node.Labels
	if nodeLabels == nil {
		nodeLabels = map[string]string{}
	}

	// Map NodeSelectorOperator to selection.Operator
	var op selection.Operator
	switch expr.Operator {
	case corev1.NodeSelectorOpIn:
		op = selection.In
	case corev1.NodeSelectorOpNotIn:
		op = selection.NotIn
	case corev1.NodeSelectorOpExists:
		op = selection.Exists
	case corev1.NodeSelectorOpDoesNotExist:
		op = selection.DoesNotExist
	case corev1.NodeSelectorOpGt:
		op = selection.GreaterThan
	case corev1.NodeSelectorOpLt:
		op = selection.LessThan
	default:
		return false
	}

	// Convert to label selector requirement for easier matching
	requirement, err := labels.NewRequirement(expr.Key, op, expr.Values)
	if err != nil {
		return false
	}

	return requirement.Matches(labels.Set(nodeLabels))
}

// nodeMatchesFieldExpression checks if a node matches a field selector requirement
func (r *ResourceSliceController) nodeMatchesFieldExpression(node *corev1.Node, expr corev1.NodeSelectorRequirement) bool {
	// For field selectors, we need to extract the field value from the node
	var fieldValue string

	switch expr.Key {
	case "metadata.name":
		fieldValue = node.Name
	case "metadata.namespace":
		fieldValue = node.Namespace
	default:
		// Unknown field
		return false
	}

	// Convert to label selector requirement for matching logic
	requirement, err := labels.NewRequirement(expr.Key, selection.Operator(expr.Operator), expr.Values)
	if err != nil {
		return false
	}

	// Create a label set with just this field
	fieldSet := labels.Set{expr.Key: fieldValue}
	return requirement.Matches(fieldSet)
}

// resourceSliceNeedsUpdate checks if an existing ResourceSlice needs to be updated based on device count or pool configuration changes
func (r *ResourceSliceController) resourceSliceNeedsUpdate(existing *resourcev1.ResourceSlice, desired *v1alpha1.ResourceSliceTemplate) bool {
	if len(existing.Spec.Devices) != len(desired.Devices) {
		return true
	}

	if existing.Spec.Pool.Name != desired.Pool.Name {
		return true
	}

	return false
}

// cleanupOrphanedResourceSlices removes ALL ResourceSlices managed by this driver
func (r *ResourceSliceController) cleanupOrphanedResourceSlices(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("resourceslice")

	// List all ResourceSlices with our management label
	resourceSlices := &resourcev1.ResourceSliceList{}
	managedByLabel := labels.SelectorFromSet(labels.Set{
		"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
	})

	if err := r.kubeClient.List(ctx, resourceSlices, &client.ListOptions{
		LabelSelector: managedByLabel,
	}); err != nil {
		return fmt.Errorf("listing resourceslices: %w", err)
	}

	// Delete all of them
	for i := range resourceSlices.Items {
		rs := &resourceSlices.Items[i]
		logger.Info("deleting orphaned resourceslice", "resourceslice", rs.Name)
		if err := r.kubeClient.Delete(ctx, rs); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "failed to delete orphaned resourceslice", "resourceslice", rs.Name)
		}
	}

	return nil
}

// cleanupUnexpectedResourceSlices removes ResourceSlices that shouldn't exist
func (r *ResourceSliceController) cleanupUnexpectedResourceSlices(ctx context.Context, expectedSlices map[string]bool) error {
	logger := log.FromContext(ctx).WithName("resourceslice")

	// List all ResourceSlices with our management label
	resourceSlices := &resourcev1.ResourceSliceList{}
	managedByLabel := labels.SelectorFromSet(labels.Set{
		"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
	})

	if err := r.kubeClient.List(ctx, resourceSlices, &client.ListOptions{
		LabelSelector: managedByLabel,
	}); err != nil {
		return fmt.Errorf("listing resourceslices: %w", err)
	}

	// Delete any that aren't in the expected set
	for i := range resourceSlices.Items {
		rs := &resourceSlices.Items[i]
		if !expectedSlices[rs.Name] {
			logger.Info("deleting unexpected resourceslice", "resourceslice", rs.Name)
			if err := r.kubeClient.Delete(ctx, rs); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "failed to delete unexpected resourceslice", "resourceslice", rs.Name)
			}
		}
	}

	return nil
}

// sanitizeDriverName converts driver name to DNS-safe format
// Example: "test.karpenter.sh" -> "test-karpenter-sh"
func sanitizeDriverName(driverName string) string {
	// Replace dots and slashes with hyphens
	sanitized := strings.ReplaceAll(driverName, ".", "-")
	sanitized = strings.ReplaceAll(sanitized, "/", "-")
	return sanitized
}
