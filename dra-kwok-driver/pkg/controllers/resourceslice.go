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

	"github.com/awslabs/operatorpkg/serrors"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/config"
)

// ResourceSliceController manages ResourceSlice lifecycle based on Node events and driver configuration
type ResourceSliceController struct {
	kubeClient       client.Client
	driverName       string
	configController *ConfigMapController
}

// NewResourceSliceController creates a new ResourceSlice controller
func NewResourceSliceController(kubeClient client.Client, driverName string, configController *ConfigMapController) *ResourceSliceController {
	return &ResourceSliceController{
		kubeClient:       kubeClient,
		driverName:       driverName,
		configController: configController,
	}
}

// SetConfigController sets the config controller reference (used when controllers are initialized in sequence)
func (r *ResourceSliceController) SetConfigController(configController *ConfigMapController) {
	r.configController = configController
}

// Register registers the controller with the manager
func (r *ResourceSliceController) Register(ctx context.Context, mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("resourceslice").
		For(&corev1.Node{}).
		Owns(&resourcev1.ResourceSlice{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 10, // Allow parallel processing of different nodes
		}).
		Complete(r)
}

// Reconcile handles Node changes and manages corresponding ResourceSlices
func (r *ResourceSliceController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Get the node
	node := &corev1.Node{}
	if err := r.kubeClient.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			// Node was deleted - clean up ResourceSlices
			log.Info("node deleted, cleaning up resourceslices", "node", req.Name)
			return r.cleanupResourceSlicesForNode(ctx, req.Name)
		}
		return reconcile.Result{}, serrors.Wrap(fmt.Errorf("getting node, %w", err), "Node", klog.KRef("", req.Name))
	}

	// Skip non-KWOK nodes
	if !r.isKWOKNode(node) {
		log.V(2).Info("skipping non-KWOK node", "node", node.Name)
		return reconcile.Result{}, nil
	}

	// Get current driver configuration
	cfg := r.configController.GetConfig()
	if cfg == nil {
		log.V(1).Info("no driver configuration available, skipping node", "node", node.Name)
		return reconcile.Result{}, nil
	}

	log.V(1).Info("reconciling node", "node", node.Name, "driver", cfg.Driver)

	// Find all matching mappings for this node
	matchingMappings := r.findMatchingMappings(node, cfg.Mappings)
	if len(matchingMappings) == 0 {
		log.V(1).Info("no matching mappings found for node", "node", node.Name)
		// Clean up any existing ResourceSlices for this node
		return r.cleanupResourceSlicesForNode(ctx, node.Name)
	}

	log.Info("found matching mappings for node",
		"node", node.Name,
		"mappingCount", len(matchingMappings),
	)

	// Create or update ResourceSlices for this node (one per mapping)
	return r.reconcileResourceSlicesForNode(ctx, node, matchingMappings, cfg.Driver)
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
		selector, err := metav1.LabelSelectorAsSelector(&mapping.NodeSelector)
		if err != nil {
			continue // Skip invalid selectors
		}

		if selector.Matches(labels.Set(node.Labels)) {
			matches = append(matches, mapping)
		}
	}
	return matches
}

// reconcileResourceSlicesForNode creates or updates ResourceSlices for a node (one per mapping)
func (r *ResourceSliceController) reconcileResourceSlicesForNode(ctx context.Context, node *corev1.Node, mappings []config.Mapping, driverName string) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Create one ResourceSlice per mapping
	for _, mapping := range mappings {
		// Create a unique ResourceSlice name for each mapping
		resourceSliceName := fmt.Sprintf("%s-devices-%s", node.Name, mapping.Name)

		// Use upstream ResourceSlice spec directly from configuration
		resourceSlice := &resourcev1.ResourceSlice{
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
			// Use the ResourceSlice spec directly from configuration
			Spec: mapping.ResourceSlice,
		}

		// Override node-specific fields - NodeName and Driver come from runtime context
		resourceSlice.Spec.NodeName = &node.Name
		resourceSlice.Spec.Driver = driverName

		// Create or update the ResourceSlice for this mapping
		existing := &resourcev1.ResourceSlice{}
		if err := r.kubeClient.Get(ctx, types.NamespacedName{Name: resourceSliceName}, existing); err != nil {
			if errors.IsNotFound(err) {
				// Create new ResourceSlice
				log.Info("creating resourceslice",
					"resourceslice", resourceSliceName,
					"node", node.Name,
					"mapping", mapping.Name,
					"devices", len(resourceSlice.Spec.Devices),
				)
				if err := r.kubeClient.Create(ctx, resourceSlice); err != nil {
					return reconcile.Result{}, serrors.Wrap(fmt.Errorf("creating resourceslice, %w", err), "ResourceSlice", klog.KRef("", resourceSliceName))
				}
			} else {
				return reconcile.Result{}, serrors.Wrap(fmt.Errorf("getting resourceslice, %w", err), "ResourceSlice", klog.KRef("", resourceSliceName))
			}
		} else {
			// Update existing ResourceSlice
			existing.Spec = resourceSlice.Spec
			existing.Labels = resourceSlice.Labels

			log.Info("updating resourceslice",
				"resourceslice", resourceSliceName,
				"node", node.Name,
				"mapping", mapping.Name,
				"devices", len(resourceSlice.Spec.Devices),
			)
			if err := r.kubeClient.Update(ctx, existing); err != nil {
				return reconcile.Result{}, serrors.Wrap(fmt.Errorf("updating resourceslice, %w", err), "ResourceSlice", klog.KRef("", resourceSliceName))
			}
		}
	}

	return reconcile.Result{}, nil
}

// cleanupResourceSlicesForNode removes all ResourceSlices associated with a node
func (r *ResourceSliceController) cleanupResourceSlicesForNode(ctx context.Context, nodeName string) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// List all ResourceSlices for this node
	resourceSlices := &resourcev1.ResourceSliceList{}
	if err := r.kubeClient.List(ctx, resourceSlices, client.MatchingLabels{
		"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
		"kwok.x-k8s.io/node":       nodeName,
	}); err != nil {
		return reconcile.Result{}, serrors.Wrap(fmt.Errorf("listing resourceslices for cleanup, %w", err), "Node", klog.KRef("", nodeName))
	}

	// Delete each ResourceSlice
	for _, rs := range resourceSlices.Items {
		log.Info("deleting resourceslice", "resourceslice", rs.Name, "node", nodeName)
		if err := r.kubeClient.Delete(ctx, &rs); err != nil && !errors.IsNotFound(err) {
			return reconcile.Result{}, serrors.Wrap(fmt.Errorf("deleting resourceslice, %w", err), "ResourceSlice", klog.KRef("", rs.Name))
		}
	}

	log.Info("cleaned up resourceslices for node", "node", nodeName, "count", len(resourceSlices.Items))
	return reconcile.Result{}, nil
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

// ReconcileAllNodes triggers reconciliation for all existing KWOK nodes
// This is called when the driver configuration changes to update ResourceSlices
func (r *ResourceSliceController) ReconcileAllNodes(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)

	log.Info("reconciling all nodes due to configuration change")

	// List all KWOK nodes
	nodes := &corev1.NodeList{}
	if err := r.kubeClient.List(ctx, nodes); err != nil {
		return fmt.Errorf("listing nodes for configuration update, %w", err)
	}

	// Count nodes that will be reconciled
	reconciledCount := 0
	for _, node := range nodes.Items {
		if r.isKWOKNode(&node) {
			// Trigger reconciliation for this node
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      node.Name,
					Namespace: node.Namespace,
				},
			})
			if err != nil {
				log.Error(err, "failed to reconcile node during configuration update", "node", node.Name)
				// Continue with other nodes even if one fails
			} else {
				reconciledCount++
			}
		}
	}

	log.Info("completed configuration update reconciliation", "nodes_reconciled", reconciledCount)
	return nil
}
