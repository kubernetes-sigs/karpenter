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

package nodeoverlay

import (
	"sync/atomic"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type priceUpdate struct {
	OverlayUpdate *string
	lowestWeight  *int32
}

type capacityUpdate struct {
	OverlayUpdate                 corev1.ResourceList
	lowestWeightCapacityResources corev1.ResourceList
	lowestWeight                  *int32
}

type instanceTypeUpdate struct {
	Price               map[string]*priceUpdate
	Capacity            *capacityUpdate
	overlayRequirements scheduling.Requirements     // Custom label requirements from the overlay
	cachedVariant       *cloudprovider.InstanceType // Pre-computed variant to avoid allocations in applyAll
}
type InstanceTypeStore struct {
	store atomic.Pointer[internalInstanceTypeStore]
}

func NewInstanceTypeStore() *InstanceTypeStore {
	publicStore := &InstanceTypeStore{
		store: atomic.Pointer[internalInstanceTypeStore]{},
	}
	publicStore.store.Store(newInternalInstanceTypeStore())
	return publicStore
}

func (s *InstanceTypeStore) UpdateStore(updatedStore *internalInstanceTypeStore) {
	s.store.Swap(updatedStore)
}

func (s *InstanceTypeStore) ApplyAll(nodePoolName string, its []*cloudprovider.InstanceType) ([]*cloudprovider.InstanceType, error) {
	internalStore := lo.FromPtr(s.store.Load())

	if !internalStore.evaluatedNodePools.Has(nodePoolName) {
		return []*cloudprovider.InstanceType{}, cloudprovider.NewUnevaluatedNodePoolError(nodePoolName)
	}

	_, ok := internalStore.updates[nodePoolName]
	if !ok {
		return its, nil
	}

	result := make([]*cloudprovider.InstanceType, 0, len(its))
	for _, it := range its {
		variants := internalStore.applyAll(nodePoolName, it)
		result = append(result, variants...)
	}
	return result, nil
}

// Apply returns a single instance type with overlays applied. This method is intended for
// use cases where only one variant is needed, such as drift detection on existing nodes.
// For scheduling, use ApplyAll instead which returns all variants including those with
// custom label requirements.
func (s *InstanceTypeStore) Apply(nodePoolName string, it *cloudprovider.InstanceType) (*cloudprovider.InstanceType, error) {
	internalStore := lo.FromPtr(s.store.Load())

	variants := internalStore.applyAll(nodePoolName, it)
	if len(variants) == 0 {
		return it, nil
	}
	// Return the first variant for backward compatibility
	return variants[0], nil
}

// InstanceTypeStore manages instance type updates for node pools.
// It maintains a nested mapping structure where:
//   - First level:  nodePoolName -> map of instance updates
//   - Second level: instanceName -> map of variant updates
//   - Third level:  variantKey (overlay requirements string) -> specific update configurations
//
// The store is used to:
//   - Track instance type modifications per node pool
//   - Validate instance configurations
//   - Update instance properties for scheduling decisions
//   - Create overlay-specific variants with custom label requirements
type internalInstanceTypeStore struct {
	updates            map[string]map[string]map[string]*instanceTypeUpdate // nodePoolName -> instanceName -> variantKey -> updates
	evaluatedNodePools sets.Set[string]                                     // The set of NodePools that were evaluated to construct this InstanceTypeStore instance
	cachedResults      map[string]map[string][]*cloudprovider.InstanceType  // nodePoolName -> instanceName -> cached result slice
}

func newInternalInstanceTypeStore() *internalInstanceTypeStore {
	return &internalInstanceTypeStore{
		updates:            map[string]map[string]map[string]*instanceTypeUpdate{},
		evaluatedNodePools: sets.Set[string]{},
		cachedResults:      map[string]map[string][]*cloudprovider.InstanceType{},
	}
}

// applyAll takes a node pool name and instance type, and returns modified copies of the instance type
// with any stored updates applied. It creates overlay-specific variants when overlays have custom label
// requirements. Each variant has:
// - The overlay's price/capacity modifications
// - The overlay's custom label requirements added to the instance type's requirements
// This ensures that pods targeting specific overlay values only match the appropriate variant.
// When overlays have custom label requirements, the base instance type is also returned so that
// pods without those specific requirements can still schedule.
func (s *internalInstanceTypeStore) applyAll(nodePoolName string, it *cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	if !s.evaluatedNodePools.Has(nodePoolName) {
		return []*cloudprovider.InstanceType{it}
	}

	if cachedNodePool, ok := s.cachedResults[nodePoolName]; ok {
		if cached, ok := cachedNodePool[it.Name]; ok {
			return cached
		}
	}

	instanceVariants, ok := s.updates[nodePoolName]
	if !ok {
		return []*cloudprovider.InstanceType{it}
	}
	variantUpdates, ok := instanceVariants[it.Name]
	if !ok {
		return []*cloudprovider.InstanceType{it}
	}

	results := make([]*cloudprovider.InstanceType, 0, len(variantUpdates)+1)
	hasCustomLabelVariant := false

	for _, update := range variantUpdates {

		var variant *cloudprovider.InstanceType
		if update.cachedVariant != nil {
			variant = update.cachedVariant
		} else {
			variant = s.applyVariant(it, update)
		}
		results = append(results, variant)

		if len(update.overlayRequirements) > 0 {
			hasCustomLabelVariant = true
		}
	}

	// If no variants were created, return the original
	if len(results) == 0 {
		return []*cloudprovider.InstanceType{it}
	}

	// If any variant has custom label requirements, also include the base instance type
	// so that pods without those specific requirements can still schedule
	if hasCustomLabelVariant {
		results = append(results, it)
	}

	return results
}

// applyVariant creates a single variant of an instance type with the given update applied
func (s *internalInstanceTypeStore) applyVariant(it *cloudprovider.InstanceType, update *instanceTypeUpdate) *cloudprovider.InstanceType {

	overriddenInstanceType := &cloudprovider.InstanceType{
		Name:     it.Name,
		Overhead: it.Overhead, // Shared - never modified
	}

	// Add overlay's custom label requirements to the instance type's requirements
	if len(update.overlayRequirements) > 0 {
		newReqs := scheduling.NewRequirements(it.Requirements.Values()...)
		newReqs.Add(update.overlayRequirements.Values()...)
		overriddenInstanceType.Requirements = newReqs
	} else {
		overriddenInstanceType.Requirements = it.Requirements // Shared - not modified
	}

	// Handle capacity overlay - only deep copy if we're modifying it
	if update.Capacity != nil && len(lo.Keys(update.Capacity.OverlayUpdate)) != 0 {
		overriddenInstanceType.Capacity = it.Capacity.DeepCopy()
		overriddenInstanceType.ApplyCapacityOverlay(update.Capacity.OverlayUpdate)
	} else {
		overriddenInstanceType.Capacity = it.Capacity // Shared - not modified
	}

	// Handle offerings - copy-on-write only for offerings that need price overlay
	if update.Price != nil {
		overriddenInstanceType.Offerings = s.applyPriceOverlays(it.Offerings, update.Price)
	} else {
		overriddenInstanceType.Offerings = it.Offerings // Shared - not modified
	}

	return overriddenInstanceType
}

// applyPriceOverlays creates a new offerings slice with selective copying:
// - Offerings that need price overlay are copied and mutated
// - Offerings without overlay share the original pointer
// This minimizes allocations while ensuring each node pool has independent pricing.
func (s *internalInstanceTypeStore) applyPriceOverlays(offerings cloudprovider.Offerings, priceUpdates map[string]*priceUpdate) cloudprovider.Offerings {
	result := make(cloudprovider.Offerings, len(offerings))
	for i, offering := range offerings {
		if overlay, ok := priceUpdates[offering.Requirements.String()]; ok {
			// This offering needs modification - create a copy
			copiedOffering := &cloudprovider.Offering{
				Requirements:        offering.Requirements, // Shared - requirements are immutable
				Price:               offering.Price,
				Available:           offering.Available,
				ReservationCapacity: offering.ReservationCapacity,
			}
			copiedOffering.ApplyPriceOverlay(lo.FromPtr(overlay.OverlayUpdate))
			result[i] = copiedOffering
		} else {
			// Not modified - share the pointer
			result[i] = offering
		}
	}
	return result
}

// updateInstanceTypeCapacity add a new Capacity overlay update to the associated instance type variant.
// NOTE: This method does not perform conflict validation. The callee must check for conflicts first.
func (i *internalInstanceTypeStore) updateInstanceTypeCapacity(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, overlayReqs scheduling.Requirements) {
	if nodeOverlay.Spec.Capacity == nil {
		return
	}

	variantKey := getCustomLabelRequirements(overlayReqs).String()
	i.ensureVariant(nodePoolName, instanceTypeName, variantKey, overlayReqs)

	update := i.updates[nodePoolName][instanceTypeName][variantKey]
	if update.Capacity == nil {
		update.Capacity = &capacityUpdate{
			OverlayUpdate:                 nodeOverlay.Spec.Capacity,
			lowestWeightCapacityResources: nodeOverlay.Spec.Capacity,
			lowestWeight:                  nodeOverlay.Spec.Weight,
		}
	} else {
		for resource, quantity := range nodeOverlay.Spec.Capacity {
			if _, foundCapacityUpdate := update.Capacity.OverlayUpdate[resource]; foundCapacityUpdate {
				continue
			}
			update.Capacity.OverlayUpdate[resource] = quantity
		}
		update.Capacity.lowestWeightCapacityResources = nodeOverlay.Spec.Capacity
		update.Capacity.lowestWeight = nodeOverlay.Spec.Weight
	}
}

func (i *internalInstanceTypeStore) isCapacityUpdateConflicting(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, overlayReqs scheduling.Requirements) bool {
	variantKey := getCustomLabelRequirements(overlayReqs).String()

	_, ok := i.updates[nodePoolName]
	if !ok {
		return false
	}
	_, ok = i.updates[nodePoolName][instanceTypeName]
	if !ok {
		return false
	}
	update, ok := i.updates[nodePoolName][instanceTypeName][variantKey]
	if !ok {
		return false
	}
	if update.Capacity == nil {
		return false
	}
	// IMPORTANT: This logic assumes NodeOverlays are processed in descending order by weight.
	if lo.FromPtr(update.Capacity.lowestWeight) != lo.FromPtr(nodeOverlay.Spec.Weight) {
		return false
	}

	for resource := range nodeOverlay.Spec.Capacity {
		if _, found := update.Capacity.lowestWeightCapacityResources[resource]; found {
			return true
		}
	}

	return false
}

// updateInstanceTypeOffering add a new Price overlay update to the associated instance type variant.
// NOTE: This method does not perform conflict validation. The callee must check for conflicts first.
func (i *internalInstanceTypeStore) updateInstanceTypeOffering(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, offerings cloudprovider.Offerings, overlayReqs scheduling.Requirements) {
	price := lo.Ternary(nodeOverlay.Spec.Price == nil, nodeOverlay.Spec.PriceAdjustment, nodeOverlay.Spec.Price)
	if price == nil {
		return
	}

	variantKey := getCustomLabelRequirements(overlayReqs).String()
	i.ensureVariant(nodePoolName, instanceTypeName, variantKey, overlayReqs)

	update := i.updates[nodePoolName][instanceTypeName][variantKey]
	for _, of := range offerings {
		if existingUpdate, foundOfferingUpdate := update.Price[of.Requirements.String()]; foundOfferingUpdate {
			existingUpdate.lowestWeight = nodeOverlay.Spec.Weight
			continue
		}
		update.Price[of.Requirements.String()] = &priceUpdate{
			OverlayUpdate: price,
			lowestWeight:  nodeOverlay.Spec.Weight,
		}
	}
}

func (i *internalInstanceTypeStore) isOfferingUpdateConflicting(nodePoolName string, instanceTypeName string, of *cloudprovider.Offering, nodeOverlay v1alpha1.NodeOverlay, overlayReqs scheduling.Requirements) bool {
	variantKey := getCustomLabelRequirements(overlayReqs).String()

	_, ok := i.updates[nodePoolName]
	if !ok {
		return false
	}
	_, ok = i.updates[nodePoolName][instanceTypeName]
	if !ok {
		return false
	}
	update, ok := i.updates[nodePoolName][instanceTypeName][variantKey]
	if !ok {
		return false
	}
	updatedOffering, ok := update.Price[of.Requirements.String()]
	if !ok {
		return false
	}
	// IMPORTANT: This logic assumes NodeOverlays are processed in descending order by weight.
	if lo.FromPtr(nodeOverlay.Spec.Weight) != lo.FromPtr(updatedOffering.lowestWeight) {
		return false
	}

	return true
}

// ensureVariant ensures the nested map structure exists for a given variant
func (i *internalInstanceTypeStore) ensureVariant(nodePoolName, instanceTypeName, variantKey string, overlayReqs scheduling.Requirements) {
	if _, ok := i.updates[nodePoolName]; !ok {
		i.updates[nodePoolName] = map[string]map[string]*instanceTypeUpdate{}
	}
	if _, ok := i.updates[nodePoolName][instanceTypeName]; !ok {
		i.updates[nodePoolName][instanceTypeName] = map[string]*instanceTypeUpdate{}
	}
	if _, ok := i.updates[nodePoolName][instanceTypeName][variantKey]; !ok {
		i.updates[nodePoolName][instanceTypeName][variantKey] = &instanceTypeUpdate{
			Price:               map[string]*priceUpdate{},
			Capacity:            &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
			overlayRequirements: getCustomLabelRequirements(overlayReqs),
		}
	}
}

// getCustomLabelRequirements filters out well-known labels and returns only custom label requirements.
// Custom labels are those not in v1.WellKnownLabels (e.g., nvidia.com/device-plugin.config).
func getCustomLabelRequirements(reqs scheduling.Requirements) scheduling.Requirements {
	customLabels := scheduling.NewRequirements()
	for key, req := range reqs {
		if v1.WellKnownLabels.Has(key) {
			continue
		}
		customLabels.Add(req)
	}
	return customLabels
}

// FinalizeCache pre-computes and caches the instance type variants for each update.
// This should be called after all overlays are stored but before the store is swapped in.
// By caching the variants and result slices, we avoid re-creating them on every applyAll call,
// significantly reducing memory allocations during scheduling.
func (i *internalInstanceTypeStore) FinalizeCache(nodePoolToInstanceTypes map[string][]*cloudprovider.InstanceType) {
	for nodePoolName, instanceTypes := range nodePoolToInstanceTypes {
		instanceUpdates, ok := i.updates[nodePoolName]
		if !ok {
			continue
		}

		if i.cachedResults[nodePoolName] == nil {
			i.cachedResults[nodePoolName] = make(map[string][]*cloudprovider.InstanceType)
		}

		for _, it := range instanceTypes {
			variantUpdates, ok := instanceUpdates[it.Name]
			if !ok {
				continue
			}

			results := make([]*cloudprovider.InstanceType, 0, len(variantUpdates)+1)
			hasCustomLabelVariant := false

			for _, update := range variantUpdates {
				update.cachedVariant = i.applyVariant(it, update)
				results = append(results, update.cachedVariant)
				if len(update.overlayRequirements) > 0 {
					hasCustomLabelVariant = true
				}
			}

			if hasCustomLabelVariant {
				results = append(results, it)
			}

			i.cachedResults[nodePoolName][it.Name] = results
		}
	}
}

func (s *InstanceTypeStore) Reset() {
	s.store.Swap(NewInstanceTypeStore().store.Load())
}
