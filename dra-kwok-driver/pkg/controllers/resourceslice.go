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

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/config"
)

// ResourceSliceController manages ResourceSlice lifecycle based on Node events and driver configuration
type ResourceSliceController struct {
	client.Client
	driverName       string
	configController *ConfigMapController
}

// NewResourceSliceController creates a new ResourceSlice controller
func NewResourceSliceController(client client.Client, driverName string, configController *ConfigMapController) *ResourceSliceController {
	return &ResourceSliceController{
		Client:           client,
		driverName:       driverName,
		configController: configController,
	}
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
	err := r.Get(ctx, req.NamespacedName, node)
	if err != nil {
		if errors.IsNotFound(err) {
			// Node was deleted - clean up ResourceSlices
			log.Info("node deleted, cleaning up resourceslices", "node", req.Name)
			return r.cleanupResourceSlicesForNode(ctx, req.Name)
		}
		log.Error(err, "failed to fetch node")
		return reconcile.Result{}, err
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

	// Find matching mapping for this node
	mapping := r.findMatchingMapping(node, cfg.Mappings)
	if mapping == nil {
		log.V(1).Info("no matching mapping found for node", "node", node.Name)
		// Clean up any existing ResourceSlices for this node
		return r.cleanupResourceSlicesForNode(ctx, node.Name)
	}

	log.Info("found matching mapping for node",
		"node", node.Name,
		"mapping", mapping.Name,
		"devices", len(mapping.ResourceSlice.Devices),
	)

	// Create or update ResourceSlices for this node
	return r.reconcileResourceSlicesForNode(ctx, node, mapping, cfg.Driver)
}

// isKWOKNode determines if a node is a KWOK node
func (r *ResourceSliceController) isKWOKNode(node *corev1.Node) bool {
	// Check for KWOK-specific labels or annotations
	if provider, ok := node.Labels["kwok.x-k8s.io/provider"]; ok && provider == "kwok" {
		return true
	}
	if nodeType, ok := node.Labels["type"]; ok && nodeType == "kwok" {
		return true
	}
	// Check for KWOK node annotation
	if _, ok := node.Annotations["kwok.x-k8s.io/node"]; ok {
		return true
	}
	return false
}

// findMatchingMapping finds a mapping that matches the node's labels
func (r *ResourceSliceController) findMatchingMapping(node *corev1.Node, mappings []config.Mapping) *config.Mapping {
	for _, mapping := range mappings {
		selector, err := metav1.LabelSelectorAsSelector(&mapping.NodeSelector)
		if err != nil {
			continue // Skip invalid selectors
		}

		if selector.Matches(labels.Set(node.Labels)) {
			return &mapping
		}
	}
	return nil
}

// reconcileResourceSlicesForNode creates or updates ResourceSlices for a specific node
func (r *ResourceSliceController) reconcileResourceSlicesForNode(ctx context.Context, node *corev1.Node, mapping *config.Mapping, driverName string) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Create ResourceSlices for each device type
	for i, deviceConfig := range mapping.ResourceSlice.Devices {
		resourceSliceName := fmt.Sprintf("%s-%s-%d", node.Name, deviceConfig.Name, i)

		resourceSlice := &resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceSliceName,
				Labels: map[string]string{
					"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
					"kwok.x-k8s.io/node":       node.Name,
					"kwok.x-k8s.io/device":     deviceConfig.Name,
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
				NodeName: &node.Name,
				Driver:   driverName,
				Pool: resourcev1.ResourcePool{
					Name:               fmt.Sprintf("%s-pool", deviceConfig.Name),
					ResourceSliceCount: 1,
				},
			},
		}

		// Create devices for this ResourceSlice
		devices := make([]resourcev1.Device, deviceConfig.Count)
		for j := 0; j < deviceConfig.Count; j++ {
			deviceName := fmt.Sprintf("%s-%d", deviceConfig.Name, j)

			// Convert attributes map to device attributes with QualifiedName keys
			deviceAttributes := make(map[resourcev1.QualifiedName]resourcev1.DeviceAttribute)
			for key, value := range deviceConfig.Attributes {
				// Use StringValue for attributes
				deviceAttributes[resourcev1.QualifiedName(key)] = resourcev1.DeviceAttribute{
					StringValue: &value,
				}
			}

			devices[j] = resourcev1.Device{
				Name:       deviceName,
				Attributes: deviceAttributes,
			}
		}
		resourceSlice.Spec.Devices = devices

		// Create or update the ResourceSlice
		existing := &resourcev1.ResourceSlice{}
		err := r.Get(ctx, types.NamespacedName{Name: resourceSliceName}, existing)
		if err != nil {
			if errors.IsNotFound(err) {
				// Create new ResourceSlice
				log.Info("creating resourceslice",
					"resourceslice", resourceSliceName,
					"node", node.Name,
					"devices", len(devices),
				)
				if err := r.Create(ctx, resourceSlice); err != nil {
					log.Error(err, "failed to create resourceslice", "resourceslice", resourceSliceName)
					return reconcile.Result{}, err
				}
			} else {
				log.Error(err, "failed to get resourceslice", "resourceslice", resourceSliceName)
				return reconcile.Result{}, err
			}
		} else {
			// Update existing ResourceSlice
			existing.Spec = resourceSlice.Spec
			existing.Labels = resourceSlice.Labels

			log.Info("updating resourceslice",
				"resourceslice", resourceSliceName,
				"node", node.Name,
				"devices", len(devices),
			)
			if err := r.Update(ctx, existing); err != nil {
				log.Error(err, "failed to update resourceslice", "resourceslice", resourceSliceName)
				return reconcile.Result{}, err
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
	err := r.List(ctx, resourceSlices, client.MatchingLabels{
		"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
		"kwok.x-k8s.io/node":       nodeName,
	})
	if err != nil {
		log.Error(err, "failed to list resourceslices for cleanup", "node", nodeName)
		return reconcile.Result{}, err
	}

	// Delete each ResourceSlice
	for _, rs := range resourceSlices.Items {
		log.Info("deleting resourceslice", "resourceslice", rs.Name, "node", nodeName)
		if err := r.Delete(ctx, &rs); err != nil && !errors.IsNotFound(err) {
			log.Error(err, "failed to delete resourceslice", "resourceslice", rs.Name)
			return reconcile.Result{}, err
		}
	}

	log.Info("cleaned up resourceslices for node", "node", nodeName, "count", len(resourceSlices.Items))
	return reconcile.Result{}, nil
}

// GetResourceSlicesForNode returns all ResourceSlices managed by this driver for a specific node
func (r *ResourceSliceController) GetResourceSlicesForNode(ctx context.Context, nodeName string) ([]*resourcev1.ResourceSlice, error) {
	resourceSlices := &resourcev1.ResourceSliceList{}
	err := r.List(ctx, resourceSlices, client.MatchingLabels{
		"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
		"kwok.x-k8s.io/node":       nodeName,
	})
	if err != nil {
		return nil, err
	}

	result := make([]*resourcev1.ResourceSlice, len(resourceSlices.Items))
	for i := range resourceSlices.Items {
		result[i] = &resourceSlices.Items[i]
	}
	return result, nil
}
