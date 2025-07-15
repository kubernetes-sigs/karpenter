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

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const conflictMessage = "conflict with another overlay"

// Controller for reconciling on node overlay resources
type Controller struct {
	kubeClient client.Client
}

func (c *Controller) Name() string {
	return "nodeoverlay.validation"
}

// NewController constructs a controller for node overlay validation
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

// Reconcile validates that all node overlays don't have conflicting requirements
func (c *Controller) Reconcile(ctx context.Context, _ *v1alpha1.NodeOverlay) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	overlayList := &v1alpha1.NodeOverlayList{}
	if err := c.kubeClient.List(ctx, overlayList); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	overlaysWithConflict := []string{}
	for _, overlayOne := range overlayList.Items {
		for _, overlayTwo := range overlayList.Items {
			if overlayOne.Name == overlayTwo.Name || lo.FromPtr(overlayOne.Spec.Weight) != lo.FromPtr(overlayTwo.Spec.Weight) || !c.hasConflictingRequirements(overlayOne, overlayTwo) {
				continue
			}
			// checks to see if there are any other conflicts against the node overlays
			// Validate against pricing and capacity allocations.
			if c.isConflictingOverlay(overlayOne, overlayTwo) {
				overlaysWithConflict = append(overlaysWithConflict, overlayOne.Name, overlayTwo.Name)
			}
		}
	}

	return reconcile.Result{}, c.updateOverlayStatuses(ctx, overlayList.Items, overlaysWithConflict)
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1alpha1.NodeOverlay{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// findConflictingRequirements checks if any node overlays with the same weight have conflicting requirements
// and returns a map of overlay names to conflict messages
func (c *Controller) hasConflictingRequirements(overlayOne v1alpha1.NodeOverlay, overlayTwo v1alpha1.NodeOverlay) bool {
	reqsA := scheduling.NewNodeSelectorRequirements(overlayOne.Spec.Requirements...)
	// For each pair of overlays, check if their requirements conflict
	reqsB := scheduling.NewNodeSelectorRequirements(overlayTwo.Spec.Requirements...)
	// Check if the requirements are compatible
	if err := reqsA.Intersects(reqsB); err == nil {
		return true
	}

	return false
}

func (c *Controller) isConflictingOverlay(overlayOne v1alpha1.NodeOverlay, overlayTwo v1alpha1.NodeOverlay) bool {
	conflictingResources := findConflictingResources(overlayOne.Spec.Capacity, overlayTwo.Spec.Capacity)
	return overlayOne.Spec.PriceAdjustment != overlayTwo.Spec.PriceAdjustment || len(conflictingResources) != 0
}

func (c *Controller) updateOverlayStatuses(ctx context.Context, overlayList []v1alpha1.NodeOverlay, overlaysWithConflict []string) error {
	for _, overlay := range overlayList {
		stored := overlay.DeepCopy()
		overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
		if lo.Contains(overlaysWithConflict, overlay.Name) {
			overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "Conflict", conflictMessage)
		}

		if !equality.Semantic.DeepEqual(stored, overlay) {
			if err := c.kubeClient.Status().Patch(ctx, &overlay, client.MergeFrom(stored)); err != nil {
				return client.IgnoreNotFound(err)
			}
		}
	}
	return nil
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
