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
	OverlayUpdate                 corev1.ResourceList
	lowestWeightCapacityResources corev1.ResourceList
	lowestWeight                  *int32
}

type InstanceTypeUpdate struct {
	Price    map[string]*PriceUpdate
	Capacity *CapacityUpdate
}

// InstanceTypeStore manages instance type updates for node pools in a thread-safe manner.
// It maintains a nested mapping structure where:
//   - First level:  nodePoolName -> map of instance updates
//   - Second level: instanceName -> specific update configurations
//
// The store is used to:
//   - Track instance type modifications per node pool
//   - Validate instance configurations
//   - Update instance properties for scheduling decisions
type InstanceTypeStore struct {
	updates map[string]map[string]*InstanceTypeUpdate // nodePoolName -> (instanceName -> updates)
	mu      sync.RWMutex                              // protects concurrent access to updates
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
		for instanceTypeName, itUpdate := range v {
			s.updates[nodePoolName][instanceTypeName] = &InstanceTypeUpdate{
				Price: lo.Assign(map[string]*PriceUpdate{}, itUpdate.Price),
				Capacity: &CapacityUpdate{
					OverlayUpdate:                 lo.Assign(corev1.ResourceList{}, itUpdate.Capacity.OverlayUpdate),
					lowestWeightCapacityResources: itUpdate.Capacity.lowestWeightCapacityResources,
					lowestWeight:                  itUpdate.Capacity.lowestWeight,
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
			OverlayUpdate:                 nodeOverlay.Spec.Capacity,
			lowestWeightCapacityResources: nodeOverlay.Spec.Capacity,
			lowestWeight:                  nodeOverlay.Spec.Weight,
		}
	} else {
		for resource, quantity := range nodeOverlay.Spec.Capacity {
			s.updates[nodePoolName][instanceTypeName].Capacity.OverlayUpdate[resource] = quantity
		}
		s.updates[nodePoolName][instanceTypeName].Capacity.lowestWeightCapacityResources = nodeOverlay.Spec.Capacity
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
		if _, found := instanceTypeUpdate.Capacity.lowestWeightCapacityResources[resource]; found {
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

func (s *InstanceTypeStore) ApplyAll(nodePoolName string, its []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	result := []*cloudprovider.InstanceType{}

	_, ok := s.updates[nodePoolName]
	if !ok {
		return its
	}

	for _, it := range its {
		result = append(result, s.Apply(nodePoolName, it))
	}
	return result
}

// Apply takes a node pool name and instance type, and returns a modified copy of the instance type
// with any stored updates applied. It checks for price and capacity updates specific to the given
// node pool and instance type, creating a deep copy of the original instance type before applying
// any overrides. If no updates exist for the node pool or instance type, returns the original
// instance type unchanged. Thread-safe through read lock usage.
func (s *InstanceTypeStore) Apply(nodePoolName string, it *cloudprovider.InstanceType) *cloudprovider.InstanceType {
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
			if overlay, ok := instanceTypeUpdate.Price[of.Requirements.String()]; ok {
				of.ApplyPriceOverlay(lo.FromPtr(overlay.OverlayUpdate))
			}
		}
	}
	if len(lo.Keys(instanceTypeUpdate.Capacity.OverlayUpdate)) != 0 {
		overriddenInstanceType.ApplyCapacityOverlay(instanceTypeUpdate.Capacity.OverlayUpdate)
	}

	return overriddenInstanceType
}

func (s *InstanceTypeStore) Reset() {
	s.updates = map[string]map[string]*InstanceTypeUpdate{}
}
