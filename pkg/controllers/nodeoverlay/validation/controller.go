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

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type InstanceTypeUpdate struct {
	instanceTypeName string
	nodePoolName     string
	overlayName      string
	Offering         map[string]*string
	Capacity         corev1.ResourceList
	weight           *int32
}

type InstanceTypeOverlayStore map[string]*InstanceTypeUpdate

func (s InstanceTypeOverlayStore) FindOverlaidInstanceTypes(nodePool v1.NodePool, nodeOverlay v1alpha1.NodeOverlay, it *cloudprovider.InstanceType) (*InstanceTypeUpdate, bool) {
	item, found := s[key(nodePool, nodeOverlay, it)]
	return item, found
}

func (s InstanceTypeOverlayStore) AddInstanceTypeUpdates(nodePool v1.NodePool, nodeOverlay v1alpha1.NodeOverlay, it *cloudprovider.InstanceType, offerings cloudprovider.Offerings) {
	// if both are not defined, price will be set to nil
	price := lo.Ternary(nodeOverlay.Spec.Price == nil, nodeOverlay.Spec.PriceAdjustment, nodeOverlay.Spec.Price)
	offeringMap := map[string]*string{}
	if price != nil {
		for _, of := range offerings {
			offeringMap[of.Requirements.String()] = price
		}
	}
	s[key(nodePool, nodeOverlay, it)] = &InstanceTypeUpdate{
		instanceTypeName: it.Name,
		nodePoolName:     nodePool.Name,
		overlayName:      nodeOverlay.Name,
		Offering:         offeringMap,
		Capacity:         nodeOverlay.Spec.Capacity,
		weight:           nodeOverlay.Spec.Weight,
	}
}

func (s InstanceTypeOverlayStore) ListInstanceTypes(it *cloudprovider.InstanceType) []*InstanceTypeUpdate {
	return lo.Filter(lo.Values(s), func(item *InstanceTypeUpdate, _ int) bool {
		return item.instanceTypeName == it.Name
	})
}

func (s InstanceTypeOverlayStore) ListOverlaidInstanceTypes(nodeOverlay v1alpha1.NodeOverlay, it *cloudprovider.InstanceType) []*InstanceTypeUpdate {
	return lo.Filter(lo.Values(s), func(item *InstanceTypeUpdate, _ int) bool {
		return item.overlayName != nodeOverlay.Name && item.instanceTypeName == it.Name
	})
}

func (s InstanceTypeOverlayStore) UpdateInstanceTypeCapacity(nodePool v1.NodePool, nodeOverlay v1alpha1.NodeOverlay, it *cloudprovider.InstanceType) {
	s[key(nodePool, nodeOverlay, it)] = &InstanceTypeUpdate{
		instanceTypeName: it.Name,
		nodePoolName:     nodePool.Name,
		overlayName:      nodeOverlay.Name,
		Offering:         map[string]*string{},
		Capacity:         corev1.ResourceList{},
		weight:           nodeOverlay.Spec.Weight,
	}
	for k, v := range nodeOverlay.Spec.Capacity {
		s[key(nodePool, nodeOverlay, it)].Capacity[k] = v
	}
}

func (s InstanceTypeOverlayStore) UpdateInstanceTypeOffering(nodePool v1.NodePool, nodeOverlay v1alpha1.NodeOverlay, it *cloudprovider.InstanceType, offering *cloudprovider.Offering) {
	overlayPriceChange := lo.Ternary(nodeOverlay.Spec.Price == nil, nodeOverlay.Spec.PriceAdjustment, nodeOverlay.Spec.Price)
	s[key(nodePool, nodeOverlay, it)] = &InstanceTypeUpdate{
		instanceTypeName: it.Name,
		nodePoolName:     nodePool.Name,
		overlayName:      nodeOverlay.Name,
		Offering:         map[string]*string{offering.Requirements.String(): overlayPriceChange},
		Capacity:         corev1.ResourceList{},
		weight:           nodeOverlay.Spec.Weight,
	}
}

func (s InstanceTypeOverlayStore) RemoveForUpdatesForOverlay(nodeOverlay v1alpha1.NodeOverlay) {
	for k, v := range s {
		if v.overlayName == nodeOverlay.Name {
			delete(s, k)
		}
	}
}

func key(nodePool v1.NodePool, nodeOverlay v1alpha1.NodeOverlay, it *cloudprovider.InstanceType) string {
	return fmt.Sprintf("%s-%s-%s", nodePool.Name, nodeOverlay.Name, it.Name)
}

func (s InstanceTypeOverlayStore) Reset() {
	for key := range s {
		delete(s, key)
	}
}

// Controller for validating NodeOverlay configuration and surfacing conflicts to the user
type Controller struct {
	kubeClient               client.Client
	cloudProvider            cloudprovider.CloudProvider
	instanceTypeOverlayStore InstanceTypeOverlayStore
}

func (c *Controller) Name() string {
	return "nodeoverlay.validation"
}

// NewController constructs a controller for node overlay validation
func NewController(kubeClient client.Client, cp cloudprovider.CloudProvider, instanceTypeOverlayStore InstanceTypeOverlayStore) *Controller {
	return &Controller{
		kubeClient:               kubeClient,
		cloudProvider:            cp,
		instanceTypeOverlayStore: instanceTypeOverlayStore,
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

	overlayList.OrderByWeight()
	for i := range overlayList.Items {
		c.instanceTypeOverlayStore.RemoveForUpdatesForOverlay(overlayList.Items[i])
		if err := overlayList.Items[i].RuntimeValidate(ctx); err != nil {
			overlayWithRuntimeValidationFailure[overlayList.Items[i].Name] = err
			continue
		}

		// Due to reserved capacity type offering being dynamically injected as part of the GetInstanceTypes call
		// We will need to make sure we are validating against each nodepool to make sure. This will ensure that
		// overlays that are targeting reserved instance offerings will be able to apply the offering.
		for _, np := range nodePoolList.Items {
			its, err := c.cloudProvider.GetInstanceTypes(ctx, &np)
			if err != nil {
				return reconcile.Result{}, fmt.Errorf("listing instance types for nodepool, %w", err)
			}
			// The additional requirements will be added to the instance type during scheduling simulation
			// Since getting instance types is done on a NodePool level, these requirnements were always assumed
			// to be allowed with these instance types.
			addNodePoolRequirements(np, its)
			overlaysWithConflict = append(overlaysWithConflict, c.checkOverlayPerNodePool(np, its, overlayList.Items[i])...)
			removeNodePoolRequirements(np, its)
		}
	}

	filterOverlaysWithConflict := lo.Filter(lo.Uniq(overlaysWithConflict), func(val string, _ int) bool { return val != "" })
	return reconcile.Result{}, c.updateOverlayStatuses(ctx, overlayList.Items, filterOverlaysWithConflict, overlayWithRuntimeValidationFailure)
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

func (c *Controller) checkOverlayPerNodePool(nodePool v1.NodePool, its []*cloudprovider.InstanceType, overlay v1alpha1.NodeOverlay) []string {
	overlaysWithConflict := []string{}
	overlayRequirements := scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...)

	for _, it := range its {
		offerings := getOverlaidOfferings(it, overlayRequirements)
		// if we are not able to find any offerings for an instance type
		// This will mean that the overlay does not select on the instance all together
		if len(offerings) != 0 {
			if len(c.instanceTypeOverlayStore.ListInstanceTypes(it)) == 0 {
				c.instanceTypeOverlayStore.AddInstanceTypeUpdates(nodePool, overlay, it, offerings)
				continue
			}

			conflictingPriceOverlay := c.isPriceAlreadyUpdated(nodePool, it, offerings, overlay)
			conflictingCapacityOverlay := c.isCapacityAlreadyUpdated(nodePool, it, overlay)
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

// isOfferingsForCompatibleInstanceType will validate that an instance type matches a set of node overlay requirements
// if true, the set of Compatible offering
func getOverlaidOfferings(it *cloudprovider.InstanceType, overlayReq scheduling.Requirements) cloudprovider.Offerings {
	if !it.Requirements.IsCompatible(overlayReq) {
		return nil
	}
	return it.Offerings.Compatible(overlayReq)
}

func (c *Controller) isPriceAlreadyUpdated(nodePool v1.NodePool, it *cloudprovider.InstanceType, offerings cloudprovider.Offerings, overlay v1alpha1.NodeOverlay) []string {
	result := []string{}
	overlayPriceChange := lo.Ternary(overlay.Spec.Price == nil, overlay.Spec.PriceAdjustment, overlay.Spec.Price)

	for _, of := range offerings {
		for _, instanceTypeUpdate := range c.instanceTypeOverlayStore.ListOverlaidInstanceTypes(overlay, it) {
			offeringPrice, foundOffering := instanceTypeUpdate.Offering[of.Requirements.String()]
			if lo.FromPtr(overlay.Spec.Weight) == lo.FromPtr(instanceTypeUpdate.weight) && (offeringPrice != nil || overlayPriceChange != nil) {
				if (lo.FromPtr(offeringPrice) == lo.FromPtr(overlayPriceChange)) || !foundOffering {
					c.instanceTypeOverlayStore.UpdateInstanceTypeOffering(nodePool, overlay, it, of)
				} else {
					result = append(result, instanceTypeUpdate.overlayName)
				}

			}
		}
	}

	return result
}

func (c *Controller) isCapacityAlreadyUpdated(nodePool v1.NodePool, it *cloudprovider.InstanceType, overlay v1alpha1.NodeOverlay) []string {
	result := []string{}

	for _, instanceTypeUpdate := range c.instanceTypeOverlayStore.ListOverlaidInstanceTypes(overlay, it) {
		if lo.FromPtr(overlay.Spec.Weight) == lo.FromPtr(instanceTypeUpdate.weight) && overlay.Spec.Capacity != nil && instanceTypeUpdate.Capacity != nil {
			resource := findConflictingResources(overlay.Spec.Capacity, instanceTypeUpdate.Capacity)
			if len(resource) == 0 {
				c.instanceTypeOverlayStore.UpdateInstanceTypeCapacity(nodePool, overlay, it)
			} else {
				result = append(result, instanceTypeUpdate.overlayName)
			}
		}
	}

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
			c.instanceTypeOverlayStore.RemoveForUpdatesForOverlay(overlay)
		}

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
