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
	"strconv"
	"strings"
	"sync"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

type PriceUpdate struct {
	overlayName   string
	OverlayUpdate *string
	Offerings     cloudprovider.Offerings
	weight        *int32
}

type CapacityUpdate struct {
	overlayName   string
	OverlayUpdate corev1.ResourceList
	weight        *int32
}

type InstanceTypeUpdate struct {
	Price    []PriceUpdate
	Capacity []CapacityUpdate
}

type InstanceTypeStore struct {
	updates map[string]map[string]*InstanceTypeUpdate
	mu      sync.RWMutex
}

func NewInstanceTypeStore() *InstanceTypeStore {
	return &InstanceTypeStore{
		updates: map[string]map[string]*InstanceTypeUpdate{},
	}
}

func (s *InstanceTypeStore) UpdateStore(updatedStore map[string]map[string]*InstanceTypeUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.updates = map[string]map[string]*InstanceTypeUpdate{}
	for nodePoolName, v := range updatedStore {
		s.updates[nodePoolName] = map[string]*InstanceTypeUpdate{}
		for overlayName, itUpdate := range v {
			s.updates[nodePoolName][overlayName] = &InstanceTypeUpdate{
				Price:    append([]PriceUpdate{}, itUpdate.Price...),
				Capacity: append([]CapacityUpdate{}, itUpdate.Capacity...),
			}
		}
	}
}

func (s *InstanceTypeStore) FindInstanceTypesUpdate(nodePoolName string, it *cloudprovider.InstanceType) (*InstanceTypeUpdate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	instanceTypeList, foundNodePool := s.updates[nodePoolName]
	if !foundNodePool {
		return nil, false
	}
	instanceTypeUpdate, foundInstanceTypeUpdate := instanceTypeList[it.Name]
	return instanceTypeUpdate, foundInstanceTypeUpdate
}

// AddInstanceTypeUpdates will add an new instance that need an update due to an overlay.
// This add an instance to the update store once after it has been validated.
func (s *InstanceTypeStore) AddInstanceTypeUpdates(nodePoolName string, nodeOverlay v1alpha1.NodeOverlay, it *cloudprovider.InstanceType, offerings cloudprovider.Offerings) {
	s.mu.Lock()
	defer s.mu.Unlock()

	Price := lo.Ternary(nodeOverlay.Spec.Price == nil, nodeOverlay.Spec.PriceAdjustment, nodeOverlay.Spec.Price)
	_, foundNodePool := s.updates[nodePoolName]
	if !foundNodePool {
		s.updates[nodePoolName] = map[string]*InstanceTypeUpdate{}
	}
	_, foundInstanceTypeUpdate := s.updates[nodePoolName][it.Name]
	if !foundInstanceTypeUpdate {
		s.updates[nodePoolName][it.Name] = &InstanceTypeUpdate{}
	}

	// We will add the expected updated to the instance update list after we have validated
	// the store contains the necessary information around the nodepool and instance type
	if nodeOverlay.Spec.Price != nil || nodeOverlay.Spec.PriceAdjustment != nil {
		s.updates[nodePoolName][it.Name].Price = append(s.updates[nodePoolName][it.Name].Price, PriceUpdate{
			overlayName:   nodeOverlay.Name,
			OverlayUpdate: Price,
			Offerings:     offerings,
			weight:        nodeOverlay.Spec.Weight,
		})
	} else if nodeOverlay.Spec.Capacity != nil {
		s.updates[nodePoolName][it.Name].Capacity = append(s.updates[nodePoolName][it.Name].Capacity, CapacityUpdate{
			overlayName:   nodeOverlay.Name,
			OverlayUpdate: nodeOverlay.Spec.Capacity,
			weight:        nodeOverlay.Spec.Weight,
		})
	}
}

// UpdateInstanceTypeCapacity add a new Capacity overlay update to the associated instance type.
// This add an Capacity update to the store once after it has been validated.
func (s *InstanceTypeStore) UpdateInstanceTypeCapacity(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, resourceToUpdate corev1.ResourceList) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, foundNodePool := s.updates[nodePoolName]
	if !foundNodePool {
		return
	}
	_, foundInstanceTypeUpdate := s.updates[nodePoolName][instanceTypeName]
	if !foundInstanceTypeUpdate {
		return
	}

	s.updates[nodePoolName][instanceTypeName].Capacity = append(s.updates[nodePoolName][instanceTypeName].Capacity, CapacityUpdate{
		overlayName:   nodeOverlay.Name,
		OverlayUpdate: resourceToUpdate,
		weight:        nodeOverlay.Spec.Weight,
	})
}

// UpdateInstanceTypeOffering add a new Price overlay update to the associated instance type.
// This add an Price update to the store once after it has been validated.
func (s *InstanceTypeStore) UpdateInstanceTypeOffering(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, offerings cloudprovider.Offerings) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, foundNodePool := s.updates[nodePoolName]
	if !foundNodePool {
		return
	}
	_, foundInstanceTypeUpdate := s.updates[nodePoolName][instanceTypeName]
	if !foundInstanceTypeUpdate {
		return
	}

	overlayPriceChange := lo.Ternary(nodeOverlay.Spec.Price == nil, nodeOverlay.Spec.PriceAdjustment, nodeOverlay.Spec.Price)
	s.updates[nodePoolName][instanceTypeName].Price = append(s.updates[nodePoolName][instanceTypeName].Price, PriceUpdate{
		overlayName:   nodeOverlay.Name,
		OverlayUpdate: overlayPriceChange,
		Offerings:     offerings,
		weight:        nodeOverlay.Spec.Weight,
	})
}

func (s *InstanceTypeStore) RemoveAllInstanceTypeUpdateFromOverlay(nodeOverlayName string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for nodePoolName := range s.updates {
		for instanceTypeNames := range s.updates[nodePoolName] {
			s.updates[nodePoolName][instanceTypeNames].Price = lo.Filter(s.updates[nodePoolName][instanceTypeNames].Price, func(update PriceUpdate, _ int) bool {
				return update.overlayName != nodeOverlayName
			})
			s.updates[nodePoolName][instanceTypeNames].Capacity = lo.Filter(s.updates[nodePoolName][instanceTypeNames].Capacity, func(update CapacityUpdate, _ int) bool {
				return update.overlayName != nodeOverlayName
			})
		}
	}
}

func (s *InstanceTypeStore) ApplyOverlayOnInstanceTypes(nodePoolName string, its []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := []*cloudprovider.InstanceType{}

	_, foundNodePoolUpdates := s.updates[nodePoolName]
	if !foundNodePoolUpdates {
		return its
	}

	for _, it := range its {
		instanceTypeUpdate, foundInstanceTypeUpdate := s.FindInstanceTypesUpdate(nodePoolName, it)
		if !foundInstanceTypeUpdate {
			result = append(result, it)
			continue
		}

		overriddenInstanceType := &cloudprovider.InstanceType{
			Name:         it.Name,
			Requirements: scheduling.NewRequirements(it.Requirements.Values()...),
			Offerings:    nodepoolutils.CopyOfferings(it.Offerings),
			Capacity:     it.Capacity.DeepCopy(),
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved:      it.Overhead.KubeReserved.DeepCopy(),
				SystemReserved:    it.Overhead.SystemReserved.DeepCopy(),
				EvictionThreshold: it.Overhead.EvictionThreshold.DeepCopy(),
			},
		}

		for _, priceUpdate := range instanceTypeUpdate.Price {
			for _, updateOfferings := range priceUpdate.Offerings {
				of := overriddenInstanceType.Offerings.Compatible(updateOfferings.Requirements)
				of[0].Price = AdjustedPrice(of[0].Price, priceUpdate.OverlayUpdate)
				of[0].ApplyOverlay()
			}
		}

		for _, capacityUpdate := range instanceTypeUpdate.Capacity {
			overriddenInstanceType.Capacity = lo.Assign(overriddenInstanceType.Capacity, capacityUpdate.OverlayUpdate)
			overriddenInstanceType.ApplyResourceOverlay()
		}
		result = append(result, overriddenInstanceType)
	}
	return result
}

func AdjustedPrice(instanceTypePrice float64, change *string) float64 {
	// if price or price adjustment is not defined, then we will return the same price
	if lo.FromPtr(change) == "" {
		return instanceTypePrice
	}

	// if price is defined, then we will return the value given in the overlay
	if !strings.HasPrefix(lo.FromPtr(change), "+") && !strings.HasPrefix(lo.FromPtr(change), "-") {
		return lo.Must(strconv.ParseFloat(lo.FromPtr(change), 64))
	}

	// Check if adjustment is a percentage
	isPercentage := strings.HasSuffix(lo.FromPtr(change), "%")
	adjustment := lo.FromPtr(change)

	var adjustedPrice float64
	if isPercentage {
		adjustment = strings.TrimSuffix(lo.FromPtr(change), "%")
		// Parse the adjustment value
		// Due to the CEL validation we can assume that
		// there will always be a valid float provided into the spec
		adjustedPrice = instanceTypePrice * (1 + (lo.Must(strconv.ParseFloat(adjustment, 64)) / 100))
	} else {
		adjustedPrice = instanceTypePrice + lo.Must(strconv.ParseFloat(adjustment, 64))
	}

	// Parse the adjustment value
	// Due to the CEL validation we can assume that
	// there will always be a valid float provided into the spec

	// Apply the adjustment
	return lo.Ternary(adjustedPrice >= 0, adjustedPrice, 0)
}
