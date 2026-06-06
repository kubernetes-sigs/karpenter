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
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1/cel"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type priceUpdate struct {
	overlayName     string
	adjustedPrice   float64
	zone            string
	capacityType    string
	OverlayUpdate   *string
	PriceExpression *cel.PriceExpression
	lowestWeight    *int32
}

type capacityUpdate struct {
	overlayName                   string
	OverlayUpdate                 corev1.ResourceList
	lowestWeightCapacityResources corev1.ResourceList
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

// PriceOverlayForOffering returns the overlay name and adjusted price for the offering
// matching the given nodePool, instanceType, zone, and capacityType. Returns ok=false
// when no price overlay applies to this combination.
func (s *InstanceTypeStore) PriceOverlayForOffering(nodePoolName, instanceTypeName, zone, capacityType string) (overlayName string, adjustedPrice float64, ok bool) {
	internalStore := lo.FromPtr(s.store.Load())
	itUpdates, exists := internalStore.updates[nodePoolName]
	if !exists {
		return "", 0, false
	}
	itUpdate, exists := itUpdates[instanceTypeName]
	if !exists {
		return "", 0, false
	}
	for _, pu := range itUpdate.Price {
		if pu.zone == zone && pu.capacityType == capacityType {
			return pu.overlayName, pu.adjustedPrice, true
		}
	}
	return "", 0, false
}

// CapacityOverlayName returns the name of the overlay that applied a capacity change
// for the given nodePool and instanceType. Returns ok=false when no capacity overlay applies.
func (s *InstanceTypeStore) CapacityOverlayName(nodePoolName, instanceTypeName string) (overlayName string, ok bool) {
	internalStore := lo.FromPtr(s.store.Load())
	itUpdates, exists := internalStore.updates[nodePoolName]
	if !exists {
		return "", false
	}
	itUpdate, exists := itUpdates[instanceTypeName]
	if !exists {
		return "", false
	}
	if itUpdate.Capacity == nil || itUpdate.Capacity.overlayName == "" {
		return "", false
	}
	return itUpdate.Capacity.overlayName, true
}

// NewTestStoreWithPriceOverlay creates an InstanceTypeStore pre-populated with a single price
// overlay entry for the given nodePool, instanceType, zone, and capacityType.
// Intended for use in tests.
func NewTestStoreWithPriceOverlay(nodePoolName, instanceTypeName, zone, capacityType, overlayName string, adjustedPrice float64) *InstanceTypeStore {
	s := NewInstanceTypeStore()
	internal := newInternalInstanceTypeStore()
	internal.updates[nodePoolName] = map[string]*instanceTypeUpdate{
		instanceTypeName: {
			Price: map[string]*priceUpdate{
				zone + "/" + capacityType: {
					overlayName:   overlayName,
					adjustedPrice: adjustedPrice,
					zone:          zone,
					capacityType:  capacityType,
				},
			},
			Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
		},
	}
	s.UpdateStore(internal)
	return s
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
		Name:         it.Name,
		Requirements: it.Requirements, // Shared - never modified
		Overhead:     it.Overhead,     // Shared - never modified
		Capacity:     it.Capacity,
	}

	// Handle capacity overlay - only deep copy if we're modifying it
	if len(instanceTypeUpdate.Capacity.OverlayUpdate) != 0 {
		// ApplyCapacityOverlay replaces Capacity with a new merged map (original untouched)
		overriddenInstanceType.ApplyCapacityOverlay(instanceTypeUpdate.Capacity.OverlayUpdate)
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
			copiedOffering := &cloudprovider.Offering{
				Requirements:        offering.Requirements, // Shared - requirements are immutable
				Price:               offering.Price,
				Available:           offering.Available,
				ReservationCapacity: offering.ReservationCapacity,
			}
			if overlay.PriceExpression != nil {
				newPrice, err := overlay.PriceExpression.Evaluate(offering.Price)
				if err != nil {
					// Evaluation errors are caught at reconcile time and surface as ValidationSucceeded=False;
					// if one slips through here, skip the offering rather than panic.
					result[i] = offering
					continue
				}
				copiedOffering.Price = newPrice
				copiedOffering.SetPriceOverlayApplied()
			} else {
				copiedOffering.ApplyPriceOverlay(lo.FromPtr(overlay.OverlayUpdate))
			}
			result[i] = copiedOffering
		} else {
			// Not modified - share the pointer
			result[i] = offering
		}
	}
	return result
}

// updateInstanceTypeCapacity add a new Capacity overlay update to the associated instance type.
// NOTE: This method does not perform conflict validation. The callee must check for conflicts first.
func (i *internalInstanceTypeStore) updateInstanceTypeCapacity(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay) {
	if nodeOverlay.Spec.Capacity == nil {
		return
	}

	_, ok := i.updates[nodePoolName]
	if !ok {
		i.updates[nodePoolName] = map[string]*instanceTypeUpdate{}
	}
	_, ok = i.updates[nodePoolName][instanceTypeName]
	if !ok {
		i.updates[nodePoolName][instanceTypeName] = &instanceTypeUpdate{Price: map[string]*priceUpdate{}, Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}}}
	}

	if i.updates[nodePoolName][instanceTypeName].Capacity == nil {
		i.updates[nodePoolName][instanceTypeName].Capacity = &capacityUpdate{
			overlayName:                   nodeOverlay.Name,
			OverlayUpdate:                 nodeOverlay.Spec.Capacity,
			lowestWeightCapacityResources: nodeOverlay.Spec.Capacity,
			lowestWeight:                  nodeOverlay.Spec.Weight,
		}
	} else {
		for resource, quantity := range nodeOverlay.Spec.Capacity {
			if _, foundCapacityUpdate := i.updates[nodePoolName][instanceTypeName].Capacity.OverlayUpdate[resource]; foundCapacityUpdate {
				continue
			}

			i.updates[nodePoolName][instanceTypeName].Capacity.OverlayUpdate[resource] = quantity
		}

		i.updates[nodePoolName][instanceTypeName].Capacity.lowestWeightCapacityResources = nodeOverlay.Spec.Capacity
		i.updates[nodePoolName][instanceTypeName].Capacity.lowestWeight = nodeOverlay.Spec.Weight
	}
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

	for resource := range nodeOverlay.Spec.Capacity {
		if _, found := instanceTypeUpdate.Capacity.lowestWeightCapacityResources[resource]; found {
			return true
		}
	}

	return false
}

// updateInstanceTypeOffering add a new Price overlay update to the associated instance type.
// NOTE: This method does not perform conflict validation. The callee must check for conflicts first.
// compiled must be non-nil when nodeOverlay.Spec.PriceExpression is set; it is compiled once by the
// caller so that both storage and evaluation share the same program.
func (i *internalInstanceTypeStore) updateInstanceTypeOffering(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, offerings cloudprovider.Offerings, compiled *cel.PriceExpression) {
	hasPriceField := nodeOverlay.Spec.Price != nil || nodeOverlay.Spec.PriceAdjustment != nil
	if !hasPriceField && compiled == nil {
		return
	}

	price := lo.Ternary(nodeOverlay.Spec.Price == nil, nodeOverlay.Spec.PriceAdjustment, nodeOverlay.Spec.Price)

	_, ok := i.updates[nodePoolName]
	if !ok {
		i.updates[nodePoolName] = map[string]*instanceTypeUpdate{}
	}
	_, ok = i.updates[nodePoolName][instanceTypeName]
	if !ok {
		i.updates[nodePoolName][instanceTypeName] = &instanceTypeUpdate{Price: map[string]*priceUpdate{}, Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}}}
	}

	for _, of := range offerings {
		if update, foundOfferingUpdate := i.updates[nodePoolName][instanceTypeName].Price[of.Requirements.String()]; foundOfferingUpdate {
			update.lowestWeight = nodeOverlay.Spec.Weight
			continue
		}
		var adjustedPrice float64
		if compiled != nil {
			// Evaluation cannot fail here; pre-check in validateAndUpdateInstanceTypeOverrides
			// already verified all offerings evaluate successfully.
			adjustedPrice, _ = compiled.Evaluate(of.Price)
		} else {
			adjustedPrice = cloudprovider.AdjustedPrice(of.Price, lo.FromPtr(price))
		}
		i.updates[nodePoolName][instanceTypeName].Price[of.Requirements.String()] = &priceUpdate{
			overlayName:     nodeOverlay.Name,
			adjustedPrice:   adjustedPrice,
			zone:            of.Zone(),
			capacityType:    of.CapacityType(),
			OverlayUpdate:   price,
			PriceExpression: compiled,
			lowestWeight:    nodeOverlay.Spec.Weight,
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
