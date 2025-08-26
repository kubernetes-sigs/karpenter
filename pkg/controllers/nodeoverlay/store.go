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
	OverlayUpdate *string
	lowestWeight  *int32
}

type CapacityUpdate struct {
	OverlayUpdate corev1.ResourceList
	lowestWeight  *int32
}

type InstanceTypeUpdate struct {
	Price    map[string]*PriceUpdate
	Capacity *CapacityUpdate
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
				Price: lo.Assign(map[string]*PriceUpdate{}, itUpdate.Price),
				Capacity: &CapacityUpdate{
					OverlayUpdate: lo.Assign(corev1.ResourceList{}, itUpdate.Capacity.OverlayUpdate),
					lowestWeight:  itUpdate.Capacity.lowestWeight,
				},
			}
		}
	}
}

// updateInstanceTypeCapacity add a new Capacity overlay update to the associated instance type.
// This add an Capacity update to the store once after it has been validated.
func (s *InstanceTypeStore) updateInstanceTypeCapacity(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if nodeOverlay.Spec.Capacity == nil {
		return
	}

	_, ok := s.updates[nodePoolName]
	if !ok {
		s.updates[nodePoolName] = map[string]*InstanceTypeUpdate{}
	}
	_, ok = s.updates[nodePoolName][instanceTypeName]
	if !ok {
		s.updates[nodePoolName][instanceTypeName] = &InstanceTypeUpdate{Price: map[string]*PriceUpdate{}, Capacity: &CapacityUpdate{OverlayUpdate: corev1.ResourceList{}}}
	}

	if s.updates[nodePoolName][instanceTypeName].Capacity == nil {
		s.updates[nodePoolName][instanceTypeName].Capacity = &CapacityUpdate{
			OverlayUpdate: nodeOverlay.Spec.Capacity,
			lowestWeight:  nodeOverlay.Spec.Weight,
		}
	} else {
		for resource, qun := range nodeOverlay.Spec.Capacity {
			s.updates[nodePoolName][instanceTypeName].Capacity.OverlayUpdate[resource] = qun
		}
		s.updates[nodePoolName][instanceTypeName].Capacity.lowestWeight = nodeOverlay.Spec.Weight
	}

}

func (s *InstanceTypeStore) isCapacityUpdateConflicting(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.updates[nodePoolName]
	if !ok {
		return false
	}
	instanceTypeUpdate, ok := s.updates[nodePoolName][instanceTypeName]
	if !ok {
		return false
	}
	if instanceTypeUpdate.Capacity == nil {
		return false
	}
	if lo.FromPtr(instanceTypeUpdate.Capacity.lowestWeight) != lo.FromPtr(nodeOverlay.Spec.Weight) {
		return false
	}

	for resource := range nodeOverlay.Spec.Capacity {
		if _, found := instanceTypeUpdate.Capacity.OverlayUpdate[resource]; found {
			return true
		}
	}

	return false
}

// updateInstanceTypeOffering add a new Price overlay update to the associated instance type.
// This add an Price update to the store once after it has been validated.
func (s *InstanceTypeStore) updateInstanceTypeOffering(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, offerings cloudprovider.Offerings) {
	s.mu.Lock()
	defer s.mu.Unlock()

	price := lo.Ternary(nodeOverlay.Spec.Price == nil, nodeOverlay.Spec.PriceAdjustment, nodeOverlay.Spec.Price)
	if price == nil {
		return
	}

	_, ok := s.updates[nodePoolName]
	if !ok {
		s.updates[nodePoolName] = map[string]*InstanceTypeUpdate{}
	}
	_, ok = s.updates[nodePoolName][instanceTypeName]
	if !ok {
		s.updates[nodePoolName][instanceTypeName] = &InstanceTypeUpdate{Price: map[string]*PriceUpdate{}, Capacity: &CapacityUpdate{OverlayUpdate: corev1.ResourceList{}}}
	}

	for _, of := range offerings {
		if update, foundOfferingUpdate := s.updates[nodePoolName][instanceTypeName].Price[of.Requirements.String()]; foundOfferingUpdate {
			update.lowestWeight = nodeOverlay.Spec.Weight
			continue
		}
		s.updates[nodePoolName][instanceTypeName].Price[of.Requirements.String()] = &PriceUpdate{
			OverlayUpdate: price,
			lowestWeight:  nodeOverlay.Spec.Weight,
		}
	}
}

func (s *InstanceTypeStore) isOfferingUpdateConflicting(nodePoolName string, instanceTypeName string, of *cloudprovider.Offering, nodeOverlay v1alpha1.NodeOverlay) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.updates[nodePoolName]
	if !ok {
		return false
	}
	_, ok = s.updates[nodePoolName][instanceTypeName]
	if !ok {
		return false
	}
	updatedOffering, ok := s.updates[nodePoolName][instanceTypeName].Price[of.Requirements.String()]
	if !ok {
		return false
	}
	if lo.FromPtr(nodeOverlay.Spec.Weight) != lo.FromPtr(updatedOffering.lowestWeight) {
		return false
	}

	return true
}

func (s *InstanceTypeStore) ApplyOverlayOnInstanceTypes(nodePoolName string, its []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	result := []*cloudprovider.InstanceType{}

	_, ok := s.updates[nodePoolName]
	if !ok {
		return its
	}

	for _, it := range its {
		result = append(result, s.ApplyOverlay(nodePoolName, it))
	}
	return result
}

func (s *InstanceTypeStore) ApplyOverlay(nodePoolName string, it *cloudprovider.InstanceType) *cloudprovider.InstanceType {
	s.mu.RLock()
	defer s.mu.RUnlock()

	instanceTypeList, ok := s.updates[nodePoolName]
	if !ok {
		return it
	}
	instanceTypeUpdate, ok := instanceTypeList[it.Name]
	if !ok {
		return it
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
	if instanceTypeUpdate.Price != nil {
		for _, of := range overriddenInstanceType.Offerings {
			if overlay, found := instanceTypeUpdate.Price[of.Requirements.String()]; found {
				of.Price = AdjustedPrice(of.Price, overlay.OverlayUpdate)
				of.ApplyOverlay()
			}
		}
	}
	if instanceTypeUpdate.Capacity != nil {
		overriddenInstanceType.Capacity = lo.Assign(overriddenInstanceType.Capacity, instanceTypeUpdate.Capacity.OverlayUpdate)
		overriddenInstanceType.ApplyCapacityOverlay()
	}

	return overriddenInstanceType
}

func (s *InstanceTypeStore) Reset() {
	s.updates = map[string]map[string]*InstanceTypeUpdate{}
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
