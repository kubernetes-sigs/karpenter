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
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Export internal types and functions for testing
// This file is only compiled during tests (due to _test.go suffix)

type PriceUpdate = priceUpdate
type CapacityUpdate = capacityUpdate
type InstanceTypeUpdate = instanceTypeUpdate
type InternalInstanceTypeStore = internalInstanceTypeStore

func NewInternalInstanceTypeStore() *InternalInstanceTypeStore {
	return newInternalInstanceTypeStore()
}

func (s *InternalInstanceTypeStore) Apply(nodePoolName string, it *cloudprovider.InstanceType) (*cloudprovider.InstanceType, error) {
	return s.apply(nodePoolName, it)
}

func (s *InternalInstanceTypeStore) UpdateInstanceTypeCapacity(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay) {
	s.updateInstanceTypeCapacity(nodePoolName, instanceTypeName, nodeOverlay)
}

func (s *InternalInstanceTypeStore) UpdateInstanceTypeOffering(nodePoolName string, instanceTypeName string, nodeOverlay v1alpha1.NodeOverlay, offerings cloudprovider.Offerings) {
	s.updateInstanceTypeOffering(nodePoolName, instanceTypeName, nodeOverlay, offerings)
}

func (s *InternalInstanceTypeStore) GetUpdates() map[string]map[string]*InstanceTypeUpdate {
	return s.updates
}

func (s *InternalInstanceTypeStore) SetUpdates(updates map[string]map[string]*InstanceTypeUpdate) {
	s.updates = updates
}

func (s *InternalInstanceTypeStore) InsertEvaluatedNodePools(nodePools ...string) {
	s.evaluatedNodePools.Insert(nodePools...)
}

func NewPriceUpdate(overlayUpdate *string, lowestWeight *int32) *PriceUpdate {
	return &PriceUpdate{
		OverlayUpdate: overlayUpdate,
		lowestWeight:  lowestWeight,
	}
}

func NewCapacityUpdate(overlayUpdate corev1.ResourceList) *CapacityUpdate {
	return &CapacityUpdate{
		OverlayUpdate: overlayUpdate,
	}
}

func NewInstanceTypeUpdate(price map[string]*PriceUpdate, capacity *CapacityUpdate) *InstanceTypeUpdate {
	return &InstanceTypeUpdate{
		Price:    price,
		Capacity: capacity,
	}
}
