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

package validation

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type conflict struct {
	price     bool
	capacity  bool
	resources []string
}

// Controller for reconciling on node overlay resources
type Controller struct {
	kubeClient client.Client
}

// NewController constructs a controller for node overlay validation
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

// Reconcile validates that all node overlays don't have conflicting requirements
func (c *Controller) Reconcile(ctx context.Context, nodeOverlay *v1alpha1.NodeOverlay) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodeoverlay.validation")
	stored := nodeOverlay.DeepCopy()

	overlays := &v1alpha1.NodeOverlayList{}
	if err := c.kubeClient.List(ctx, overlays, client.MatchingFields{"spec.weight": fmt.Sprintf("%d", lo.FromPtr(nodeOverlay.Spec.Weight))}); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, err
		}
	}
	lo.Filter(overlays.Items, func(o v1alpha1.NodeOverlay, _ int) bool {
		return o.Name != nodeOverlay.Name
	})

	conflictMessage := ""
	conflictingOverlays := c.hasConflictingRequirements(nodeOverlay, lo.Filter(overlays.Items, func(o v1alpha1.NodeOverlay, _ int) bool { return o.Name != nodeOverlay.Name }))
	for _, o := range conflictingOverlays {
		conflict, err := c.areConflictingOverlay(ctx, nodeOverlay, o)
		if err != nil {
			return reconcile.Result{}, err
		}
		if conflict != nil {
			if conflict.price {
				conflictMessage = fmt.Sprintf("%s, conflict on the priceAdjustment with overlay: %s", conflictMessage, o.Name)
			}
			if conflict.capacity {
				conflictMessage = fmt.Sprintf("%s, conflict on the capacity with overlay: %s on resource: %s", conflictMessage, o.Name, conflict.resources)
			}
		}
	}

	if conflictMessage != "" {
		nodeOverlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "Conflict", conflictMessage)
	} else {
		nodeOverlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
	}

	if !equality.Semantic.DeepEqual(stored, nodeOverlay) {
		if err := c.kubeClient.Status().Patch(ctx, nodeOverlay, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}

	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeoverlay.validation").
		For(&v1alpha1.NodeOverlay{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func (c *Controller) areConflictingOverlay(ctx context.Context, currentOverlay *v1alpha1.NodeOverlay, possibleOverlayToUpdate v1alpha1.NodeOverlay) (*conflict, error) {
	stored := possibleOverlayToUpdate.DeepCopy()
	conf := &conflict{
		price:    false,
		capacity: false,
	}

	if currentOverlay.Spec.PriceAdjustment != possibleOverlayToUpdate.Spec.PriceAdjustment {
		possibleOverlayToUpdate.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "Conflict", fmt.Sprintf("conflict on the priceAdjustment with overlay: %s", currentOverlay.Name))
		conf.price = true
	}
	if resources := findConflictingResources(currentOverlay.Spec.Capacity, possibleOverlayToUpdate.Spec.Capacity); len(resources) != 0 {
		possibleOverlayToUpdate.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "Conflict", fmt.Sprintf("conflict on the capacity with overlay: %s on resource: %s", currentOverlay.Name, resources))
		conf.capacity = true
		conf.resources = resources
	}

	if !equality.Semantic.DeepEqual(stored, possibleOverlayToUpdate) {
		if err := c.kubeClient.Status().Patch(ctx, &possibleOverlayToUpdate, client.MergeFrom(stored)); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
	}

	return lo.Ternary(conf.price || conf.capacity, conf, nil), nil
}

// findConflictingRequirements checks if any node overlays with the same weight have conflicting requirements
// and returns a map of overlay names to conflict messages
func (c *Controller) hasConflictingRequirements(overlay *v1alpha1.NodeOverlay, possibleConflictingOverlies []v1alpha1.NodeOverlay) []v1alpha1.NodeOverlay {
	conflictingOverlays := []v1alpha1.NodeOverlay{}
	reqsA := scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...)

	// For each pair of overlays, check if their requirements conflict
	for x := range len(possibleConflictingOverlies) {
		reqsB := scheduling.NewNodeSelectorRequirements(possibleConflictingOverlies[x].Spec.Requirements...)

		// Check if the requirements are compatible
		if err := reqsA.Intersects(reqsB); err == nil {
			conflictingOverlays = append(conflictingOverlays, possibleConflictingOverlies[x])
		}
	}

	return conflictingOverlays
}

func findConflictingResources(capacityOne corev1.ResourceList, capacityTwo corev1.ResourceList) []string {
	result := []string{}

	for key, quantity := range capacityOne {
		if _, ok := capacityTwo[key]; ok && !quantity.Equal(capacityTwo[key]) {
			result = append(result, string(key))
		}
	}

	return result
}
