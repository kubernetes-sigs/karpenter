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

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const conflictMessage = "conflict with another overlay"

// Controller for reconciling on node overlay resources
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func (c *Controller) Name() string {
	return "nodeoverlay.validation"
}

// NewController constructs a controller for node overlay validation
func NewController(kubeClient client.Client, cp cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cp,
	}
}

// Reconcile validates that all node overlays don't have conflicting requirements
func (c *Controller) Reconcile(ctx context.Context, _ *v1alpha1.NodeOverlay) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	overlayList := &v1alpha1.NodeOverlayList{}
	if err := c.kubeClient.List(ctx, overlayList); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	overlayWithRuntimeValidationFailure := map[string]error{}
	overlaysWithConflict := []string{}
	// This validation function will check every node overlay against every other overlay
	// to validate that their are no conflicts. If needed, it is possible for us to build a conflict graph that will
	// to optimizes the validation process. Today, we do not expect customer to have more the 1000 overlays in their cluster, making this simple solution bellow
	// acceptable for the use-cases.
	for _, overlayOne := range overlayList.Items {
		if err := overlayOne.RuntimeValidate(ctx); err != nil {
			overlayWithRuntimeValidationFailure[overlayOne.Name] = err
			continue
		}
		for _, overlayTwo := range overlayList.Items {
			if err := overlayTwo.RuntimeValidate(ctx); err != nil {
				overlayWithRuntimeValidationFailure[overlayTwo.Name] = err
				continue
			}
			// We are only comparing overlays with the same weight as that is they only way for conflicts to
			// occur by customers.
			if overlayOne.Name == overlayTwo.Name || lo.FromPtr(overlayOne.Spec.Weight) != lo.FromPtr(overlayTwo.Spec.Weight) {
				continue
			}
			requirementsConflict, err := c.hasConflictingRequirements(ctx, overlayOne, overlayTwo)
			if err != nil {
				return reconcile.Result{}, err
			}
			if !requirementsConflict {
				continue
			}
			// checks to see if there are any other conflicts against the node overlays
			// Validate against pricing and capacity allocations.
			if c.isConflictingOverlay(overlayOne, overlayTwo) {
				overlaysWithConflict = append(overlaysWithConflict, overlayOne.Name, overlayTwo.Name)
			}
		}
	}

	return reconcile.Result{}, c.updateOverlayStatuses(ctx, overlayList.Items, overlaysWithConflict, overlayWithRuntimeValidationFailure)
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		// The reconciled overlay does not matter in this case as one reconcile loop
		// will compare every overlay against every other overlay in the cluster.
		For(&v1alpha1.NodeOverlay{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
			RateLimiter:             reasonable.RateLimiter(),
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// hasConflictingRequirements checks if two node overlays override either the same instance type or offering
// with a single NodePool. We are scoping the check to instance types from the configurations that are defined within the NodePool.
func (c *Controller) hasConflictingRequirements(ctx context.Context, overlayOne v1alpha1.NodeOverlay, overlayTwo v1alpha1.NodeOverlay) (bool, error) {
	overlayRequirementsA := scheduling.NewNodeSelectorRequirements(overlayOne.Spec.Requirements...)
	// For each pair of overlays, check if their requirements conflict
	overlayRequirementsB := scheduling.NewNodeSelectorRequirements(overlayTwo.Spec.Requirements...)
	// // Check if the requirements are compatible for each instance type
	nodePoolList := &v1.NodePoolList{}
	err := c.kubeClient.List(ctx, nodePoolList)
	if err != nil {
		return false, fmt.Errorf("listing nodepool, %w", err)
	}
	for _, np := range nodePoolList.Items {
		its, err := c.cloudProvider.GetInstanceTypes(ctx, &np)
		if err != nil {
			return false, fmt.Errorf("listing instance types from nodepool, %w", err)
		}
		// To save on extra reconciliation, we validate all instance types for a NodePool
		// as opposed to checking the instance that are defined with a NodePool requirement set
		for _, it := range its {
			if compatibleInstanceType(it, overlayRequirementsA, overlayRequirementsB) {
				return true, nil
			}
		}
	}

	return false, nil
}

// compatibleInstanceType will validate if an instance type and its offerings offerings are compatible with two node overlay requirements.
func compatibleInstanceType(it *cloudprovider.InstanceType, overlayReqA scheduling.Requirements, overlayReqB scheduling.Requirements) bool {
	if it.Requirements.Compatible(overlayReqA) == nil && it.Requirements.Compatible(overlayReqB) == nil {
		if overlayReqA.Keys().HasAny(v1.WellKnownLabelsForOfferings.UnsortedList()...) && overlayReqB.Keys().HasAny(v1.WellKnownLabelsForOfferings.UnsortedList()...) {
			for _, of := range it.Offerings {
				if of.Requirements.Compatible(overlayReqA) == nil && of.Requirements.Compatible(overlayReqB) == nil {
					return true
				}
			}
		} else {
			return true
		}
	}
	return false
}

// isConflictingOverlay validates if a the price, priceAdjustment, and capacity field all alter the same resources
func (c *Controller) isConflictingOverlay(overlayOne v1alpha1.NodeOverlay, overlayTwo v1alpha1.NodeOverlay) bool {
	conflictingResources := findConflictingResources(overlayOne.Spec.Capacity, overlayTwo.Spec.Capacity)
	return lo.FromPtr(overlayOne.Spec.PriceAdjustment) != lo.FromPtr(overlayTwo.Spec.PriceAdjustment) ||
		lo.FromPtr(overlayOne.Spec.Price) != lo.FromPtr(overlayTwo.Spec.Price) ||
		len(conflictingResources) != 0
}

func (c *Controller) updateOverlayStatuses(ctx context.Context, overlayList []v1alpha1.NodeOverlay, overlaysWithConflict []string, overlayWithRuntimeValidationFailure map[string]error) error {
	for _, overlay := range overlayList {
		stored := overlay.DeepCopy()
		overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
		if err, ok := overlayWithRuntimeValidationFailure[overlay.Name]; ok {
			overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "RuntimeValidation", err.Error())
		} else if lo.Contains(overlaysWithConflict, overlay.Name) {
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

// findConflictingResources compares two resource field with each other, and validated that
// the same resource is not set to different values within the resource list
func findConflictingResources(capacityOne corev1.ResourceList, capacityTwo corev1.ResourceList) []string {
	result := []string{}
	for key, quantity := range capacityOne {
		if _, ok := capacityTwo[key]; ok && !quantity.Equal(capacityTwo[key]) {
			result = append(result, string(key))
		}
	}
	return result
}
