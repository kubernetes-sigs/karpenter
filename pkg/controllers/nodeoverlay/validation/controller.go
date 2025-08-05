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
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nodeoverlayutils "sigs.k8s.io/karpenter/pkg/utils/nodeoverlay"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// Controller for validating NodeOverlay configuration and surfacing conflicts to the user
type Controller struct {
	kubeClient        client.Client
	cloudProvider     cloudprovider.CloudProvider
	instanceTypeStore *InstanceTypeStore

	temporaryStore *InstanceTypeStore
}

func (c *Controller) Name() string {
	return "nodeoverlay.validation"
}

// NewController constructs a controller for node overlay validation
func NewController(kubeClient client.Client, cp cloudprovider.CloudProvider, instanceTypeStore *InstanceTypeStore) *Controller {
	return &Controller{
		kubeClient:        kubeClient,
		cloudProvider:     cp,
		instanceTypeStore: instanceTypeStore,
		temporaryStore:    NewInstanceTypeStore(),
	}
}

// Reconcile validates that all node overlays don't have conflicting requirements
func (c *Controller) Reconcile(ctx context.Context, _ *v1alpha1.NodeOverlay) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	overlayList := &v1alpha1.NodeOverlayList{}
	nodePoolList := &v1.NodePoolList{}
	if err := c.kubeClient.List(ctx, overlayList); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodeoverlays, %w", err)
	}
	err := c.kubeClient.List(ctx, nodePoolList)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodepool, %w", err)
	}

	overlayWithRuntimeValidationFailure := map[string]error{}
	overlaysWithConflict := []string{}
	c.temporaryStore = NewInstanceTypeStore()

	overlayList.OrderByWeight()
	for i := range overlayList.Items {
		if err := overlayList.Items[i].RuntimeValidate(ctx); err != nil {
			overlayWithRuntimeValidationFailure[overlayList.Items[i].Name] = err
			continue
		}

		// Due to reserved capacity type offering being dynamically injected as part of the GetInstanceTypes call
		// We will need to make sure we are validating against each nodepool to make sure. This will ensure that
		// overlays that are targeting reserved instance offerings will be able to apply the offering.
		for _, np := range nodePoolList.Items {
			its, err := nodepoolutils.PullInstanceTypes(ctx, c.cloudProvider, &np)
			if err != nil {
				return reconcile.Result{}, err
			}
			// The additional requirements will be added to the instance type during scheduling simulation
			// Since getting instance types is done on a NodePool level, these requirements were always assumed
			// to be allowed with these instance types.
			addNodePoolRequirements(np, its)
			overlaysWithConflict = append(overlaysWithConflict, c.checkOverlayPerNodePool(np.Name, its, overlayList.Items[i])...)
			removeNodePoolRequirements(np, its)
		}
	}

	filterOverlaysWithConflict := lo.Filter(lo.Uniq(overlaysWithConflict), func(val string, _ int) bool { return val != "" })
	return reconcile.Result{RequeueAfter: 6 * time.Hour}, c.updateOverlayStatuses(ctx, overlayList.Items, filterOverlaysWithConflict, overlayWithRuntimeValidationFailure)
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		// The reconciled overlay does not matter in this case as one reconcile loop
		// will compare every overlay against every other overlay in the cluster.
		For(&v1alpha1.NodeOverlay{}).
		Watches(
			&v1.NodePool{},
			nodeoverlayutils.NodePoolEventHandler(c.kubeClient),
		).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
			RateLimiter:             reasonable.RateLimiter(),
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func (c *Controller) checkOverlayPerNodePool(nodePoolName string, its []*cloudprovider.InstanceType, overlay v1alpha1.NodeOverlay) []string {
	overlaysWithConflict := []string{}
	overlayRequirements := scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...)

	for _, it := range its {
		offerings := getOverlaidOfferings(it, overlayRequirements)
		// if we are not able to find any offerings for an instance type
		// This will mean that the overlay does not select on the instance all together
		if len(offerings) != 0 {
			// if we are not able to find an update, we will add the update to the store and skip the conflict check
			instanceTypeUpdate, foundUpdate := c.temporaryStore.FindInstanceTypesUpdate(nodePoolName, it)
			if !foundUpdate {
				c.temporaryStore.AddInstanceTypeUpdates(nodePoolName, overlay, it, offerings)
				continue
			}

			conflictingPriceOverlay := c.isPriceAlreadyUpdated(nodePoolName, it.Name, instanceTypeUpdate, offerings, overlay)
			conflictingCapacityOverlay := c.isCapacityAlreadyUpdated(nodePoolName, it.Name, instanceTypeUpdate, overlay)
			// When we find an instance type that is matches a set offering, we will track that based on the
			// overlay that is applied
			if len(conflictingPriceOverlay) != 0 || len(conflictingCapacityOverlay) != 0 {
				overlaysWithConflict = append(overlaysWithConflict, overlay.Name)
				overlaysWithConflict = append(overlaysWithConflict, conflictingPriceOverlay...)
				overlaysWithConflict = append(overlaysWithConflict, conflictingCapacityOverlay...)

			}
		}
	}

	return overlaysWithConflict
}

// getOverlaidOfferings will validate that an instance type matches a set of node overlay requirements
// if true, the set of Compatible offering. This function effectively assumes, that if the if there are no offering returned then
// the instance type is not Compatible with the overlay requirements. In cases, were an capacity overlay is being intended to be applied
// based on the offerings, this will be an all or nothing operation. If one offering matches to the requirements
// it will be applied at the instance type level or all the offerings.
func getOverlaidOfferings(it *cloudprovider.InstanceType, overlayReq scheduling.Requirements) cloudprovider.Offerings {
	if !it.Requirements.IsCompatible(overlayReq) {
		return nil
	}
	return it.Offerings.Compatible(overlayReq)
}

func (c *Controller) isPriceAlreadyUpdated(nodePoolName string, instanceTypeName string, itUpdate *InstanceTypeUpdate, offerings cloudprovider.Offerings, overlay v1alpha1.NodeOverlay) []string {
	result := []string{}
	if overlay.Spec.Price == nil && overlay.Spec.PriceAdjustment == nil {
		return result
	}

	priceWithMatchingWeight := lo.Filter(itUpdate.Price, func(update PriceUpdate, _ int) bool {
		return lo.FromPtr(update.weight) == lo.FromPtr(overlay.Spec.Weight)
	})

	// We will validate that overlays with the same weight are not updating the same offering
	for _, priceUpdate := range priceWithMatchingWeight {
		offeringsSet := lo.Map(offerings, func(of *cloudprovider.Offering, _ int) string {
			return of.Requirements.String()
		})
		priceOfferingsSet := lo.Map(priceUpdate.Offerings, func(of *cloudprovider.Offering, _ int) string {
			return of.Requirements.String()
		})

		if len(lo.FindDuplicates(append(offeringsSet, priceOfferingsSet...))) != 0 {
			result = append(result, priceUpdate.overlayName)
		}
	}

	if len(result) != 0 {
		return result
	}

	// We only want to consider resources that are not applied by a different overlay that has a higher weight.
	// We know in this case that the resource value will not be applied that that it is not in conflict with the any other overlays
	updatesWithGreaterWeights := lo.Filter(itUpdate.Price, func(update PriceUpdate, _ int) bool {
		return lo.FromPtr(update.weight) > lo.FromPtr(overlay.Spec.Weight)
	})
	offeringToUpdate := cloudprovider.Offerings{}
	for _, offering := range offerings {
		if lo.EveryBy(updatesWithGreaterWeights, func(update PriceUpdate) bool {
			return !update.Offerings.HasCompatible(offering.Requirements)
		}) {
			offeringToUpdate = append(offeringToUpdate, offering)
		}

	}
	c.temporaryStore.UpdateInstanceTypeOffering(nodePoolName, instanceTypeName, overlay, offeringToUpdate)
	return result
}

func (c *Controller) isCapacityAlreadyUpdated(nodePoolName string, instanceTypeName string, itUpdate *InstanceTypeUpdate, overlay v1alpha1.NodeOverlay) []string {
	result := []string{}
	if overlay.Spec.Capacity == nil {
		return result
	}

	capacityWithMatchingWeight := lo.Filter(itUpdate.Capacity, func(update CapacityUpdate, _ int) bool {
		return lo.FromPtr(update.weight) == lo.FromPtr(overlay.Spec.Weight)
	})

	// We will validate that overlays with the same weight are not updating the same offering
	for _, capacityUpdate := range capacityWithMatchingWeight {
		// Once we find resource that matches the update
		// we will short circuit since we know that will fail
		for resource := range overlay.Spec.Capacity {
			_, matchingResource := capacityUpdate.OverlayUpdate[resource]
			if matchingResource {
				result = append(result, capacityUpdate.overlayName)
				break
			}
		}
	}
	if len(result) != 0 {
		return result
	}

	// We only want to consider resources that are not applied by a different overlay that has a higher weight.
	// We know in this case that the resource value will not be applied that that it is not in conflict with the any other overlays
	updatesWithGreaterWeights := lo.Filter(itUpdate.Capacity, func(update CapacityUpdate, _ int) bool {
		return lo.FromPtr(update.weight) > lo.FromPtr(overlay.Spec.Weight)
	})
	resourceToUpdate := corev1.ResourceList{}
	for resource, quantity := range overlay.Spec.Capacity {
		if lo.EveryBy(updatesWithGreaterWeights, func(update CapacityUpdate) bool {
			_, found := update.OverlayUpdate[resource]
			return !found
		}) {
			resourceToUpdate[resource] = quantity
		}
	}

	c.temporaryStore.UpdateInstanceTypeCapacity(nodePoolName, instanceTypeName, overlay, resourceToUpdate)
	return result
}

func (c *Controller) updateOverlayStatuses(ctx context.Context, overlayList []v1alpha1.NodeOverlay, overlaysWithConflict []string, overlayWithRuntimeValidationFailure map[string]error) error {
	var errs []error
	for _, overlay := range overlayList {
		stored := overlay.DeepCopy()
		overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
		if err, ok := overlayWithRuntimeValidationFailure[overlay.Name]; ok {
			overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "RuntimeValidation", err.Error())
		} else if lo.Contains(overlaysWithConflict, overlay.Name) {
			overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "Conflict", "conflict with another overlay")
			c.temporaryStore.RemoveAllInstanceTypeUpdateFromOverlay(overlay.Name)
		}

		c.instanceTypeStore.UpdateStore(c.temporaryStore.updates)

		if !equality.Semantic.DeepEqual(stored, overlay) {
			// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
			// can cause races due to the fact that it fully replaces the list on a change
			// Here, we are updating the status condition list
			if err := c.kubeClient.Status().Patch(ctx, &overlay, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); !errors.IsConflict(client.IgnoreNotFound(err)) {
				errs = append(errs, err)
			}
		}
	}
	return multierr.Combine(errs...)
}

func addNodePoolRequirements(nodePool v1.NodePool, its []*cloudprovider.InstanceType) {
	for _, it := range its {
		nodePoolReq := scheduling.NewRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpIn, nodePool.Name)
		nodeClassReq := scheduling.NewRequirement(v1.NodeClassLabelKey(nodePool.Spec.Template.Spec.NodeClassRef.GroupKind()), corev1.NodeSelectorOpIn, nodePool.Spec.Template.Spec.NodeClassRef.Name)
		it.Requirements.Add(scheduling.NewLabelRequirements(nodePool.Spec.Template.ObjectMeta.Labels).Values()...)
		it.Requirements.Add(nodePoolReq, nodeClassReq)
	}
}

func removeNodePoolRequirements(nodePool v1.NodePool, its []*cloudprovider.InstanceType) {
	for _, it := range its {
		removeReq := []string{v1.NodePoolLabelKey, v1.NodeClassLabelKey(nodePool.Spec.Template.Spec.NodeClassRef.GroupKind())}
		removeReq = append(removeReq, lo.Keys(nodePool.Spec.Template.ObjectMeta.Labels)...)
		it.Requirements = lo.OmitByKeys(it.Requirements, removeReq)
	}
}
