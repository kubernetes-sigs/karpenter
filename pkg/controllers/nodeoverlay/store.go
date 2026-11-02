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

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type priceUpdate struct {
	OverlayUpdate *string
	lowestWeight  *int32
}

type capacityUpdate struct {
	// OverlayUpdate holds the fixed amounts from spec.capacity.
	OverlayUpdate corev1.ResourceList
	// FlexibleUpdate holds the ceilings from spec.maxCapacity. Its keys never overlap with OverlayUpdate.
	FlexibleUpdate corev1.ResourceList
	// lowestWeightCapacityResources holds the resource names that the lowest weight overlay set in either field.
	lowestWeightCapacityResources sets.Set[corev1.ResourceName]
	lowestWeight                  *int32
}

type instanceTypeUpdate struct {
	Price    map[string]*priceUpdate
	Capacity *capacityUpdate
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

	result := make([]*cloudprovider.InstanceType, 0, len(its))

	_, ok := internalStore.updates[nodePoolName]
	if !ok {
		return its, nil
	}

	for _, it := range its {
		result = append(result, internalStore.apply(nodePoolName, it))
	}
	return result, nil
}

func (s *InstanceTypeStore) Apply(nodePoolName string, it *cloudprovider.InstanceType) (*cloudprovider.InstanceType, error) {
	internalStore := lo.FromPtr(s.store.Load())

	if !internalStore.evaluatedNodePools.Has(nodePoolName) {
		return &cloudprovider.InstanceType{}, cloudprovider.NewUnevaluatedNodePoolError(nodePoolName)
	}

	return internalStore.apply(nodePoolName, it), nil
}

// HasFlexibleCapacity reports whether a NodeOverlay sets maxCapacity for the instance type in the NodePool.
func (s *InstanceTypeStore) HasFlexibleCapacity(nodePoolName string, instanceTypeName string) bool {
	internalStore := lo.FromPtr(s.store.Load())
	update, ok := internalStore.updates[nodePoolName][instanceTypeName]
	return ok && update.Capacity != nil && len(update.Capacity.FlexibleUpdate) != 0
}

// NodePoolHasFlexibleCapacity reports whether a NodeOverlay sets maxCapacity for any instance type in the NodePool.
func (s *InstanceTypeStore) NodePoolHasFlexibleCapacity(nodePoolName string) bool {
	internalStore := lo.FromPtr(s.store.Load())
	return lo.SomeBy(lo.Values(internalStore.updates[nodePoolName]), func(update *instanceTypeUpdate) bool {
		return update.Capacity != nil && len(update.Capacity.FlexibleUpdate) != 0
	})
}

// InstanceTypeStore manages instance type updates for node pools.
// It maintains a nested mapping structure where:
//   - First level:  nodePoolName -> map of instance updates
//   - Second level: instanceName -> specific update configurations
//
// The store is used to:
//   - Track instance type modifications per node pool
//   - Validate instance configurations
//   - Update instance properties for scheduling decisions
type internalInstanceTypeStore struct {
	updates            map[string]map[string]*instanceTypeUpdate // nodePoolName -> (instanceName -> updates)
	evaluatedNodePools sets.Set[string]                          // The set of NodePools that were evaluated to construct this InstanceTypeStore instance
}

func newInternalInstanceTypeStore() *internalInstanceTypeStore {
	return &internalInstanceTypeStore{
		updates:            map[string]map[string]*instanceTypeUpdate{},
		evaluatedNodePools: sets.Set[string]{},
	}
}

// Apply takes a node pool name and instance type, and returns a modified copy of the instance type
// with any stored updates applied. It uses a selective copy-on-write strategy to minimize memory usage:
// - Shared: Requirements and Overhead (never modified, safe to share)
// - Selective copy: Offerings (only copied if price overlay applied)
// - Selective copy: Capacity (only copied if capacity overlay applied)
func (s *internalInstanceTypeStore) apply(nodePoolName string, it *cloudprovider.InstanceType) *cloudprovider.InstanceType {
	instanceTypeList, ok := s.updates[nodePoolName]
	if !ok {
		return it
	}
	instanceTypeUpdate, ok := instanceTypeList[it.Name]
	if !ok {
		return it
	}

	// Create a shallow copy of the instance type, sharing immutable fields
	overriddenInstanceType := &cloudprovider.InstanceType{
		Name:                   it.Name,
		Requirements:           it.Requirements,           // Shared - never modified
		Overhead:               it.Overhead,               // Shared - never modified
		VolumeAttachmentLimits: it.VolumeAttachmentLimits, // Shared - never modified
		DynamicResources:       it.DynamicResources,       // Shared - never modified
		Capacity:               it.Capacity,
	}

	// Handle capacity overlay - only deep copy if we're modifying it
	if len(instanceTypeUpdate.Capacity.OverlayUpdate) != 0 {
		// ApplyCapacityOverlay replaces Capacity with a new merged map (original untouched)
		overriddenInstanceType.ApplyCapacityOverlay(instanceTypeUpdate.Capacity.OverlayUpdate)
	}
	if len(instanceTypeUpdate.Capacity.FlexibleUpdate) != 0 {
		// ApplyMaxCapacityOverlay replaces Capacity with a new merged map and keeps the store's map as read-only
		overriddenInstanceType.ApplyMaxCapacityOverlay(instanceTypeUpdate.Capacity.FlexibleUpdate)
	}

	// Handle offerings - copy-on-write only for offerings that need price overlay
	if len(instanceTypeUpdate.Price) != 0 {
		overriddenInstanceType.Offerings = s.applyPriceOverlays(it.Offerings, instanceTypeUpdate.Price)
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
				CapacityOverride:    offering.CapacityOverride, // Shared - never modified
				OverheadOverride:    offering.OverheadOverride, // Shared - never modified
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

// updateInstanceTypeCapacity add a new Capacity overlay update to the associated instance type.
// For each resource, the first overlay to set it wins, whether it set the resource in capacity or maxCapacity.
// NOTE: This method does not perform conflict validation. The callee must check for conflicts first.
func (i *internalInstanceTypeStore) updateInstanceTypeCapacity(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay) {
	if nodeOverlay.Spec.Capacity == nil && nodeOverlay.Spec.MaxCapacity == nil {
		return
	}

	_, ok := i.updates[nodePoolName]
	if !ok {
		i.updates[nodePoolName] = map[string]*instanceTypeUpdate{}
	}
	_, ok = i.updates[nodePoolName][instanceTypeName]
	if !ok {
		i.updates[nodePoolName][instanceTypeName] = &instanceTypeUpdate{Price: map[string]*priceUpdate{}, Capacity: newCapacityUpdate()}
	}
	update := i.updates[nodePoolName][instanceTypeName].Capacity
	if update.OverlayUpdate == nil {
		update.OverlayUpdate = corev1.ResourceList{}
	}
	if update.FlexibleUpdate == nil {
		update.FlexibleUpdate = corev1.ResourceList{}
	}

	for resource, quantity := range nodeOverlay.Spec.Capacity {
		if update.hasResource(resource) {
			continue
		}
		update.OverlayUpdate[resource] = quantity
	}
	for resource, quantity := range nodeOverlay.Spec.MaxCapacity {
		if update.hasResource(resource) {
			continue
		}
		update.FlexibleUpdate[resource] = quantity
	}

	update.lowestWeightCapacityResources = capacityResourceNames(nodeOverlay)
	update.lowestWeight = nodeOverlay.Spec.Weight
}

func newCapacityUpdate() *capacityUpdate {
	return &capacityUpdate{OverlayUpdate: corev1.ResourceList{}, FlexibleUpdate: corev1.ResourceList{}}
}

// hasResource reports whether a higher weight overlay already set the resource in either field.
func (c *capacityUpdate) hasResource(resource corev1.ResourceName) bool {
	if _, ok := c.OverlayUpdate[resource]; ok {
		return true
	}
	_, ok := c.FlexibleUpdate[resource]
	return ok
}

// capacityResourceNames returns the resource names that the overlay sets in capacity or maxCapacity.
func capacityResourceNames(nodeOverlay v1alpha1.NodeOverlay) sets.Set[corev1.ResourceName] {
	return sets.New(append(lo.Keys(nodeOverlay.Spec.Capacity), lo.Keys(nodeOverlay.Spec.MaxCapacity)...)...)
}

func (i *internalInstanceTypeStore) isCapacityUpdateConflicting(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay) bool {
	_, ok := i.updates[nodePoolName]
	if !ok {
		return false
	}
	instanceTypeUpdate, ok := i.updates[nodePoolName][instanceTypeName]
	if !ok {
		return false
	}
	if instanceTypeUpdate.Capacity == nil {
		return false
	}
	// IMPORTANT: This logic assumes NodeOverlays are processed in descending order by weight.
	if lo.FromPtr(instanceTypeUpdate.Capacity.lowestWeight) != lo.FromPtr(nodeOverlay.Spec.Weight) {
		return false
	}

	return capacityResourceNames(nodeOverlay).HasAny(instanceTypeUpdate.Capacity.lowestWeightCapacityResources.UnsortedList()...)
}

// updateInstanceTypeOffering add a new Price overlay update to the associated instance type.
// NOTE: This method does not perform conflict validation. The callee must check for conflicts first.
func (i *internalInstanceTypeStore) updateInstanceTypeOffering(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, offerings cloudprovider.Offerings) {
	price := lo.Ternary(nodeOverlay.Spec.Price == nil, nodeOverlay.Spec.PriceAdjustment, nodeOverlay.Spec.Price)
	if price == nil {
		return
	}

	_, ok := i.updates[nodePoolName]
	if !ok {
		i.updates[nodePoolName] = map[string]*instanceTypeUpdate{}
	}
	_, ok = i.updates[nodePoolName][instanceTypeName]
	if !ok {
		i.updates[nodePoolName][instanceTypeName] = &instanceTypeUpdate{Price: map[string]*priceUpdate{}, Capacity: newCapacityUpdate()}
	}

	for _, of := range offerings {
		requirementKey := of.Requirements.String()
		if update, foundOfferingUpdate := i.updates[nodePoolName][instanceTypeName].Price[requirementKey]; foundOfferingUpdate {
			update.lowestWeight = nodeOverlay.Spec.Weight
			continue
		}
		i.updates[nodePoolName][instanceTypeName].Price[requirementKey] = &priceUpdate{
			OverlayUpdate: price,
			lowestWeight:  nodeOverlay.Spec.Weight,
		}
	}
}

func (i *internalInstanceTypeStore) isOfferingUpdateConflicting(nodePoolName string, instanceTypeName string, of *cloudprovider.Offering, nodeOverlay v1alpha1.NodeOverlay) bool {
	_, ok := i.updates[nodePoolName]
	if !ok {
		return false
	}
	_, ok = i.updates[nodePoolName][instanceTypeName]
	if !ok {
		return false
	}
	updatedOffering, ok := i.updates[nodePoolName][instanceTypeName].Price[of.Requirements.String()]
	if !ok {
		return false
	}
	// IMPORTANT: This logic assumes NodeOverlays are processed in descending order by weight.
	if lo.FromPtr(nodeOverlay.Spec.Weight) != lo.FromPtr(updatedOffering.lowestWeight) {
		return false
	}

	return true
}

func (s *InstanceTypeStore) Reset() {
	s.store.Swap(NewInstanceTypeStore().store.Load())
}
