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

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/singleton"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/apis/v1alpha1"
)

// ResourceSliceController manages ResourceSlice lifecycle based on periodic polling of nodes and DRAConfig CRD
type ResourceSliceController struct {
	kubeClient client.Client
}

// NewResourceSliceController creates a new ResourceSlice controller
func NewResourceSliceController(kubeClient client.Client) *ResourceSliceController {
	return &ResourceSliceController{
		kubeClient: kubeClient,
	}
}

const pollingPeriod = 30 * time.Second

func (r *ResourceSliceController) Name() string {
	return "resourceslice"
}

func (r *ResourceSliceController) Register(_ context.Context, mgr manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).
		Named(r.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(r))
}

func (r *ResourceSliceController) Reconcile(ctx context.Context) (reconciler.Result, error) {
	if err := r.reconcileAllNodes(ctx); err != nil {
		return reconciler.Result{}, err
	}
	return reconciler.Result{RequeueAfter: pollingPeriod}, nil
}

// reconcileAllNodes reconciles ResourceSlices for all KWOK nodes in the cluster
func (r *ResourceSliceController) reconcileAllNodes(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("resourceslice")

	// List all DRAConfig CRDs (cluster-scoped, no namespace filter)
	draConfigs := &v1alpha1.DRAConfigList{}
	err := r.kubeClient.List(ctx, draConfigs)
	if err != nil {
		return fmt.Errorf("listing DRAConfigs: %w", err)
	}

	if len(draConfigs.Items) == 0 {
		logger.V(1).Info("no DRAConfigs found, cleaning up all ResourceSlices")
		return r.cleanupOrphanedResourceSlices(ctx)
	}

	// Group pools by driver name (merging pools from multiple configs)
	poolsByDriver := r.groupPoolsByDriver(draConfigs.Items)

	logger.Info("discovered drivers from DRAConfigs",
		"driver_count", len(poolsByDriver),
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
	for driverName, pools := range poolsByDriver {
		expected, errs := r.processDriver(ctx, driverName, pools, nodes.Items)
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
		"drivers", len(poolsByDriver),
		"errors", errorCount,
		"expected_slices", len(expectedResourceSlices),
	)

	if errorCount > 0 {
		return fmt.Errorf("reconciliation completed with %d errors", errorCount)
	}
	return nil
}

// groupPoolsByDriver merges pools from all DRAConfigs by their driver name
func (r *ResourceSliceController) groupPoolsByDriver(configs []v1alpha1.DRAConfig) map[string][]v1alpha1.Pool {
	poolsByDriver := make(map[string][]v1alpha1.Pool)
	for _, cfg := range configs {
		poolsByDriver[cfg.Spec.Driver] = append(poolsByDriver[cfg.Spec.Driver], cfg.Spec.Pools...)
	}
	return poolsByDriver
}

// processDriver processes all nodes for a single driver, returning expected ResourceSlice names and error count
func (r *ResourceSliceController) processDriver(
	ctx context.Context,
	driverName string,
	pools []v1alpha1.Pool,
	nodes []corev1.Node,
) ([]string, int) {
	logger := log.FromContext(ctx).WithName("resourceslice")
	logger.V(1).Info("reconciling driver", "driver", driverName)

	var expectedNames []string
	errorCount := 0
	kwokNodeCount := 0

	// Process each node for this driver
	for i := range nodes {
		node := &nodes[i]
		if !r.isKWOKNode(node) {
			continue
		}
		kwokNodeCount++

		// Reconcile this node with merged pools for this driver
		expected, err := r.reconcileNodeResourceSlicesForDriver(ctx, node, driverName, pools)
		if err != nil {
			logger.Error(err, "failed to reconcile node", "node", node.Name, "driver", driverName)
			errorCount++
			continue
		}

		expectedNames = append(expectedNames, expected...)
	}

	logger.Info("completed driver reconciliation",
		"driver", driverName,
		"kwok_nodes", kwokNodeCount,
		"pools", len(pools))

	return expectedNames, errorCount
}

// reconcileNodeResourceSlicesForDriver processes a single node for a specific driver
func (r *ResourceSliceController) reconcileNodeResourceSlicesForDriver(
	ctx context.Context,
	node *corev1.Node,
	driverName string,
	pools []v1alpha1.Pool,
) ([]string, error) {
	logger := log.FromContext(ctx).WithName("resourceslice").WithValues("node", node.Name, "driver", driverName)

	// Find all matching pools for this node
	matchingPools := r.findMatchingPools(node, pools)

	if len(matchingPools) == 0 {
		logger.V(1).Info("no matching pools found for node")
		return nil, nil
	}

	logger.V(1).Info("found matching pools", "count", len(matchingPools))

	var expectedNames []string

	// Process each pool. Each pool may have multiple resourceSlice templates,
	// and each template becomes one ResourceSlice per matching node.
	// All ResourceSlices in a pool share the same pool name and ResourceSliceCount.
	for _, pool := range matchingPools {
		driverSanitized := sanitizeDriverName(driverName)

		// Pool name is auto-generated as <driver>/<node> to prevent overlap across nodes
		poolName := fmt.Sprintf("%s/%s", driverName, node.Name)

		sliceCount := int64(len(pool.ResourceSlices))

		for sliceIndex, sliceTemplate := range pool.ResourceSlices {
			// ResourceSlice naming: <driver-sanitized>-<nodename>-<pool-name> (single entry)
			// or <driver-sanitized>-<nodename>-<pool-name>-<index> (multiple entries)
			resourceSliceName := fmt.Sprintf("%s-%s-%s", driverSanitized, node.Name, pool.Name)
			if sliceCount > 1 {
				resourceSliceName = fmt.Sprintf("%s-%d", resourceSliceName, sliceIndex)
			}
			expectedNames = append(expectedNames, resourceSliceName)

			// Check if ResourceSlice exists
			existing := &resourcev1.ResourceSlice{}
			err := r.kubeClient.Get(ctx, types.NamespacedName{Name: resourceSliceName}, existing)

			if err == nil {
				// ResourceSlice exists - check if update is needed
				if !r.resourceSliceNeedsUpdate(existing, sliceTemplate.Devices, sliceCount) {
					continue
				}

				// Update needed - bump generation and update in place
				oldGeneration := existing.Spec.Pool.Generation
				newGeneration := oldGeneration + 1

				existing.Spec.Devices = sliceTemplate.Devices
				existing.Spec.Pool = resourcev1.ResourcePool{
					Name:               poolName,
					ResourceSliceCount: sliceCount,
					Generation:         newGeneration,
				}

				logger.Info("updating resourceslice",
					"resourceslice", resourceSliceName,
					"old_generation", oldGeneration,
					"new_generation", newGeneration,
					"devices", len(existing.Spec.Devices),
					"slice_count", sliceCount,
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
							"test.karpenter.sh/managed-by": "dra-kwok-driver",
							"test.karpenter.sh/node":       node.Name,
							"test.karpenter.sh/pool":       pool.Name,
							"test.karpenter.sh/driver":     driverName,
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
					Spec: resourcev1.ResourceSliceSpec{
						Driver:   driverName,
						NodeName: &node.Name,
						Pool: resourcev1.ResourcePool{
							Name:               poolName,
							ResourceSliceCount: sliceCount,
							Generation:         0,
						},
						Devices: sliceTemplate.Devices,
					},
				}

				logger.Info("creating resourceslice",
					"resourceslice", desired.Name,
					"pool", desired.Labels["test.karpenter.sh/pool"],
					"driver", driverName,
					"pool_name", poolName,
					"devices", len(desired.Spec.Devices),
					"slice_count", sliceCount,
				)
				if err := r.kubeClient.Create(ctx, desired); err != nil {
					return nil, serrors.Wrap(fmt.Errorf("creating resourceslice, %w", err), "ResourceSlice", klog.KRef("", desired.Name))
				}
			} else {
				return nil, serrors.Wrap(fmt.Errorf("getting resourceslice, %w", err), "ResourceSlice", klog.KRef("", resourceSliceName))
			}
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

// findMatchingPools returns all pools that match the given node
func (r *ResourceSliceController) findMatchingPools(node *corev1.Node, pools []v1alpha1.Pool) []v1alpha1.Pool {
	var matches []v1alpha1.Pool

	for _, pool := range pools {
		if r.nodeSelectorMatches(node, pool.NodeSelectorTerms) {
			matches = append(matches, pool)
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

// resourceSliceNeedsUpdate checks if an existing ResourceSlice needs to be updated
// based on device count or slice count changes
func (r *ResourceSliceController) resourceSliceNeedsUpdate(existing *resourcev1.ResourceSlice, desiredDevices []resourcev1.Device, desiredSliceCount int64) bool {
	return len(existing.Spec.Devices) != len(desiredDevices) || existing.Spec.Pool.ResourceSliceCount != desiredSliceCount
}

// cleanupOrphanedResourceSlices removes ALL ResourceSlices managed by this driver
func (r *ResourceSliceController) cleanupOrphanedResourceSlices(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("resourceslice")

	// List all ResourceSlices with our management label
	resourceSlices := &resourcev1.ResourceSliceList{}
	managedByLabel := labels.SelectorFromSet(labels.Set{
		"test.karpenter.sh/managed-by": "dra-kwok-driver",
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
		"test.karpenter.sh/managed-by": "dra-kwok-driver",
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
