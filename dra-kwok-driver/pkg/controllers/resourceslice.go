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
	driverName string
	namespace  string // Namespace where DRAConfig CRD is located
}

// NewResourceSliceController creates a new ResourceSlice controller
func NewResourceSliceController(kubeClient client.Client, driverName string, namespace string) *ResourceSliceController {
	return &ResourceSliceController{
		kubeClient: kubeClient,
		driverName: driverName,
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

	// Get THE single DRAConfig CRD for this driver
	draConfig := &v1alpha1.DRAConfig{}
	err := r.kubeClient.Get(ctx, types.NamespacedName{
		Name:      r.driverName,
		Namespace: r.namespace,
	}, draConfig)

	if errors.IsNotFound(err) {
		logger.V(1).Info("no DRAConfig found for driver, cleaning up all ResourceSlices", "driver", r.driverName)
		// No config exists - cleanup all ResourceSlices
		return r.cleanupOrphanedResourceSlices(ctx)
	}

	if err != nil {
		return serrors.Wrap(fmt.Errorf("getting DRAConfig, %w", err), "DRAConfig", klog.KRef(r.namespace, r.driverName))
	}

	// Verify driver name matches (sanity check)
	if draConfig.Spec.Driver != r.driverName {
		logger.Error(fmt.Errorf("driver name mismatch"), "DRAConfig driver field doesn't match controller driver",
			"crd_driver", draConfig.Spec.Driver,
			"controller_driver", r.driverName)
		return fmt.Errorf("driver name mismatch: CRD has driver=%s but controller expects driver=%s",
			draConfig.Spec.Driver, r.driverName)
	}

	logger.V(1).Info("starting reconciliation cycle",
		"driver", draConfig.Spec.Driver,
		"mappings", len(draConfig.Spec.Mappings))

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
		expected, err := r.reconcileNodeResourceSlices(ctx, &node, draConfig)
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
func (r *ResourceSliceController) reconcileNodeResourceSlices(ctx context.Context, node *corev1.Node, draConfig *v1alpha1.DRAConfig) ([]string, error) {
	logger := log.FromContext(ctx).WithName("resourceslice").WithValues("node", node.Name)

	// Find all matching mappings for this node
	matchingMappings := r.findMatchingMappings(node, draConfig.Spec.Mappings)

	if len(matchingMappings) == 0 {
		logger.V(1).Info("no matching mappings found for node")
		// No ResourceSlices should exist for this node
		return nil, nil
	}

	logger.V(1).Info("found matching mappings", "count", len(matchingMappings))

	// Track which ResourceSlices should exist
	var expectedNames []string

	// Process each mapping
	for _, mapping := range matchingMappings {
		// ResourceSlice naming: test-karpenter-sh-<nodename>-<mapping-name>
		// Sanitize driver name: "test.karpenter.sh" -> "test-karpenter-sh"
		driverSanitized := sanitizeDriverName(r.driverName)
		resourceSliceName := fmt.Sprintf("%s-%s-%s", driverSanitized, node.Name, mapping.Name)
		expectedNames = append(expectedNames, resourceSliceName)

		// Check if ResourceSlice exists
		existing := &resourcev1.ResourceSlice{}
		err := r.kubeClient.Get(ctx, types.NamespacedName{Name: resourceSliceName}, existing)

		if err == nil {
			// ResourceSlice exists - check if update is needed
			if !r.resourceSliceNeedsUpdate(existing, &mapping.ResourceSlice) {
				// No change needed
				continue
			}

			// Update needed - bump generation and update in place
			oldGeneration := existing.Spec.Pool.Generation
			newGeneration := oldGeneration + 1

			// Update the spec with new configuration using ToResourceSliceSpec
			existing.Spec = mapping.ResourceSlice.ToResourceSliceSpec(r.driverName)
			existing.Spec.NodeName = &node.Name
			existing.Spec.Pool.Generation = newGeneration

			logger.Info("updating resourceslice in place with new generation",
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
				Spec: mapping.ResourceSlice.ToResourceSliceSpec(r.driverName),
			}

			// Override node-specific fields
			desired.Spec.NodeName = &node.Name
			desired.Spec.Pool.Generation = 0 // Start at generation 0

			logger.Info("creating resourceslice",
				"resourceslice", desired.Name,
				"mapping", desired.Labels["kwok.x-k8s.io/mapping"],
				"generation", 0,
				"devices", len(desired.Spec.Devices),
			)
			if err := r.kubeClient.Create(ctx, desired); err != nil {
				return nil, serrors.Wrap(fmt.Errorf("creating resourceslice, %w", err), "ResourceSlice", klog.KRef("", desired.Name))
			}
		} else {
			// Unexpected error
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
		return serrors.Wrap(fmt.Errorf("listing resourceslices, %w", err), "ResourceSliceList")
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
		return serrors.Wrap(fmt.Errorf("listing resourceslices, %w", err), "ResourceSliceList")
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
