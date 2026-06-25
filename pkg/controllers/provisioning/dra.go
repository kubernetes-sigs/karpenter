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

package provisioning

import (
	"context"
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

// gatherResourceSlices lists the in-cluster ResourceSlices and filters them down to the set the DRA allocator should
// treat as the published universe of in-cluster devices. A slice is dropped when it is owned by a Node that is not
// represented by an initialized, included stateNode:
//   - Slices owned by nodes that aren't in the provided stateNode set (deleting nodes, disruption candidates) are
//     excluded, since those nodes' devices shouldn't be considered persistent capacity.
//   - Slices owned by uninitialized nodes are excluded. Until a node is initialized its devices are represented by
//     template devices (see the NodeClaim adapters), so including its published slices would double-count them.
//
// Ownership is determined via metadata.ownerReferences (Kind == "Node"), not spec.nodeName, which only indicates
// accessibility. Slices with no Node owner reference (e.g. cluster-wide slices) are always included.
func (p *Provisioner) gatherResourceSlices(ctx context.Context, stateNodes []*state.StateNode) ([]dynamicresources.ResourceSlice, error) {
	sliceList := &resourcev1.ResourceSliceList{}
	if err := p.kubeClient.List(ctx, sliceList); err != nil {
		return nil, fmt.Errorf("listing resourceslices, %w", err)
	}

	includedNodeNames := sets.New[string]()
	for _, n := range stateNodes {
		if n.Node == nil {
			continue
		}
		if n.Initialized() {
			includedNodeNames.Insert(n.Name())
		}
	}

	slices := make([]dynamicresources.ResourceSlice, 0, len(sliceList.Items))
	for i := range sliceList.Items {
		slice := &sliceList.Items[i]
		if ownerNode, ok := nodeOwnerName(slice); ok && !includedNodeNames.Has(ownerNode) {
			continue
		}
		slices = append(slices, dynamicresources.NewAPIServerSlice(slice))
	}
	return slices, nil
}

// nodeOwnerName returns the name of the Node that owns the ResourceSlice via an ownerReference, along with whether such
// an owner reference exists. Slices published by node-local DRA drivers carry a Node owner reference; cluster-wide
// slices may not.
func nodeOwnerName(slice *resourcev1.ResourceSlice) (string, bool) {
	for _, ref := range slice.OwnerReferences {
		if ref.Kind == "Node" {
			return ref.Name, true
		}
	}
	return "", false
}

// gatherAllocatedDevices reads the set of in-cluster allocated devices from the deviceallocation controller and filters
// out devices (or portions of shared-device capacity) that should be treated as available for reallocation:
//   - Devices allocated exclusively by deleting pods (all consumers are in deletingPodUIDs) are excluded, since those
//     pods are migrating off their nodes.
//   - Releasable devices with no live consumers (unowned) are excluded.
//   - For multi-allocatable (shared) devices that survive the above, the consumed capacity of any claim whose pods are
//     all deleting is subtracted, so a deleting pod's share of a shared device is freed even when live pods keep the
//     device itself in use.
//
// The remaining devices form the allocator's immutable seed set of already-allocated in-cluster devices, split into
// exclusively-allocated devices and the consumed capacity of multi-allocatable (shared) devices.
func (p *Provisioner) gatherAllocatedDevices(ctx context.Context, deletingPodUIDs sets.Set[types.UID]) (dynamicresources.AllocatedDeviceState, error) {
	seq, err := p.deviceAllocationController.AllocatedDevices(ctx)
	if err != nil {
		return dynamicresources.AllocatedDeviceState{}, fmt.Errorf("getting allocated devices, %w", err)
	}
	state := dynamicresources.AllocatedDeviceState{
		ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
		ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{},
	}
	for id, meta := range seq {
		// A device with no live consumers that every referencing claim considers releasable is available.
		if meta.Releasable && len(meta.PodUIDs) == 0 {
			continue
		}
		// A device consumed only by deleting pods is available for reallocation.
		if len(meta.PodUIDs) > 0 && allConsumersDeleting(meta.PodUIDs, deletingPodUIDs) {
			continue
		}
		if meta.Shared {
			// Free just the deleting pods' share of the device's consumed capacity. The device as a whole survives the
			// all-consumers-deleting check above (live pods remain), but each contribution whose pods are all deleting
			// is migrating off and its capacity should be available for reallocation.
			effective := effectiveConsumedCapacity(meta, deletingPodUIDs)
			if len(effective) == 0 {
				// Every dimension was freed (or there was no live consumption left); treat the device as available.
				continue
			}
			state.ConsumedCapacity[id] = effective
			continue
		}
		state.ExclusiveDevices.Insert(id)
	}
	return state, nil
}

// effectiveConsumedCapacity computes a shared device's consumed capacity with the contributions of fully-deleting
// claims removed. It starts from the aggregated capacity and subtracts each contribution whose reserving pods are all
// deleting, dropping any dimension that reaches zero or below. The returned map is freshly allocated and never mutates
// the controller's metadata.
func effectiveConsumedCapacity(meta deviceallocation.DeviceMetadata, deletingPodUIDs sets.Set[types.UID]) map[resourcev1.QualifiedName]resource.Quantity {
	effective := make(map[resourcev1.QualifiedName]resource.Quantity, len(meta.ConsumedCapacity))
	for dim, qty := range meta.ConsumedCapacity {
		effective[dim] = qty.DeepCopy()
	}
	for _, contribution := range meta.Contributions {
		// A contribution reserved by no pods (e.g. a non-pod consumer) is never treated as deleting — only contributions
		// whose entire pod set is being deleted are freed.
		if len(contribution.PodUIDs) == 0 || !allConsumersDeleting(contribution.PodUIDs, deletingPodUIDs) {
			continue
		}
		for dim, consumed := range contribution.ConsumedCapacity {
			remaining, ok := effective[dim]
			if !ok {
				continue
			}
			remaining.Sub(consumed)
			if remaining.Sign() <= 0 {
				delete(effective, dim)
				continue
			}
			effective[dim] = remaining
		}
	}
	return effective
}

func allConsumersDeleting(podUIDs []types.UID, deletingPodUIDs sets.Set[types.UID]) bool {
	for _, uid := range podUIDs {
		if !deletingPodUIDs.Has(uid) {
			return false
		}
	}
	return true
}
