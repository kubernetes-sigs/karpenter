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

package dynamicresources

import (
	"context"

	"github.com/samber/lo"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// AllocationTracker is an opaque object used to track the allocation status of individual devices. It can be mutated
// via Commit and ReleaseInstanceTypes, which update allocation status based on NodeClaim constraints applied by the
// scheduler. Allocation status is queried via IsAllocated. Not that the allocation status of a device is not
// independent - it's dependent on the NodeClaim and InstanceType we're attempting to perform the allocation against.
// This is a unique property of Karpenter's NodeClaim model for DRA.
type AllocationTracker struct {
	// PreallocatedDevices represents the devices which are already allocated on the API server. This value is immutable
	// after set during Allocator construction.
	PreallocatedDevices sets.Set[DeviceID]

	// InflightClusterAllocations contains the allocation metadata by device for a given device ID. Note that entries in
	// this structure are not immutable - as instance types are released by NodeClaims, devices also have the potential to
	// be released and removed from the map.
	InflightClusterAllocations map[DeviceID]*InflightAllocationMetadata

	// InflightClusterAllocationsByNodeClaim is the inverse of inflightClusterAllocations, tracking the allocated devices
	// by NodeClaim and device ID. This is an acceleration data structure used to lookup the impacted devices when an
	// instance type is released for a NodeClaim.
	InflightClusterAllocationsByNodeClaim map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]

	InflightTemplateAllocations map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]

	// RemainingCounters tracks the remaining counter budgets per pool. Initialized lazily per pool
	// (total - preallocated consumption), then decremented on Commit and incremented on Release.
	// Map: poolKey → counterSetName → counterName → remaining counter.
	RemainingCounters map[PoolKey]map[string]map[string]resourcev1.Counter
	// countersByNodeClaimIT stores per-NodeClaim per-IT counter consumption for precise release
	// when instance types are pruned.
	// Map: nodeClaimID → instanceTypeID → poolKey → counterSetName → counterName → consumed counter.
	countersByNodeClaimIT map[NodeClaimID]map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter

	// templateRemainingCounters tracks the remaining template counter budgets per (NodeClaim, IT, Pool).
	// Initialized lazily with the full SharedCounters budget on first access, then decremented on Commit.
	// Separate from RemainingCounters because template and in-cluster pools can share the same PoolKey,
	// template counters are per-IT (no pessimistic-max), and they don't affect global RemainingCounters.
	// Map: nodeClaimID → instanceTypeID → poolKey → counterSetName → counterName → remaining counter.
	templateRemainingCounters map[NodeClaimID]map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter
}

func NewAllocationTracker(preallocatedDevices ...cloudprovider.DeviceID) *AllocationTracker {
	converted := make(sets.Set[DeviceID], len(preallocatedDevices))
	for i := range preallocatedDevices {
		converted.Insert(DeviceID{
			DeviceID: preallocatedDevices[i],
		})
	}
	return &AllocationTracker{
		PreallocatedDevices:                   converted,
		InflightClusterAllocations:            make(map[DeviceID]*InflightAllocationMetadata),
		InflightClusterAllocationsByNodeClaim: make(map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]),
		InflightTemplateAllocations:           make(map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]),
		RemainingCounters:                     make(map[PoolKey]map[string]map[string]resourcev1.Counter),
		countersByNodeClaimIT:                 make(map[NodeClaimID]map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter),
		templateRemainingCounters:             make(map[NodeClaimID]map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter),
	}
}

// InflightAllocationMetadata constains the nodeClaim that a device was allocated for and the set of instance types for
// that nodeclaim. This set of instance types may be a subset of the nodeclaim's total instance types - if so the
// allocation is released when all instance types are released.
type InflightAllocationMetadata struct {
	// NodeClaimID represents the NodeClaim that the ResourceClaim is indirectly bound to (through the pod). This device
	// may have only been allocated to satisfy the claim for a subset of instance types on the NodeClaim.
	NodeClaimID NodeClaimID
	// InstanceTypes represents the instance types for the NodeClaim that allocated this device to satisfy a ResourceClaim
	InstanceTypes sets.Set[InstanceTypeID]
}

func (at *AllocationTracker) Commit(alloc *allocation) {
	for it, deviceIDs := range alloc.deviceIDsByIT {
		for _, id := range deviceIDs {
			if id.Template {
				at.insertAllocation(at.InflightTemplateAllocations, id, alloc.nodeClaimID, it)
				continue
			}
			at.insertAllocation(at.InflightClusterAllocationsByNodeClaim, id, alloc.nodeClaimID, it)

			meta, ok := at.InflightClusterAllocations[id]
			if ok {
				if meta.NodeClaimID != alloc.nodeClaimID {
					panic("device is already allocated for a different nodeclaim")
				}
				if meta.InstanceTypes.Has(it) {
					panic("device is already allocated for instance type")
				}
				meta.InstanceTypes.Insert(it)
			} else {
				meta = &InflightAllocationMetadata{
					NodeClaimID:   alloc.nodeClaimID,
					InstanceTypes: make(sets.Set[InstanceTypeID]),
				}
				meta.InstanceTypes.Insert(it)
				at.InflightClusterAllocations[id] = meta
			}
		}
	}
	at.commitCounters(alloc.nodeClaimID, alloc.counterConsumptionByIT)
	at.commitTemplateCounters(alloc.nodeClaimID, alloc.templateCounterConsumptionByIT)
}

func (at *AllocationTracker) insertAllocation(
	allocationMap map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID],
	deviceID DeviceID,
	nodeClaimID NodeClaimID,
	instanceTypeID InstanceTypeID,
) {
	nodeClaimAllocs, ok := allocationMap[nodeClaimID]
	if !ok {
		nodeClaimAllocs = make(map[InstanceTypeID]sets.Set[DeviceID])
		allocationMap[nodeClaimID] = nodeClaimAllocs
	}
	itAllocs, ok := nodeClaimAllocs[instanceTypeID]
	if !ok {
		itAllocs = make(sets.Set[DeviceID])
		nodeClaimAllocs[instanceTypeID] = itAllocs
	}
	if itAllocs.Has(deviceID) {
		panic("device is already allocated for instance type")
	}
	itAllocs.Insert(deviceID)
}

// commitCounters stores per-IT counter consumption and decrements remaining counters by the
// delta between the new accumulated pessimistic max and the old one.
func (at *AllocationTracker) commitCounters(nodeClaimID NodeClaimID, newCounterConsumptionByIT map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter) {
	if len(newCounterConsumptionByIT) == 0 {
		return
	}
	storedCounterSetsByIT, ok := at.countersByNodeClaimIT[nodeClaimID]
	if !ok {
		storedCounterSetsByIT = make(map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter)
		at.countersByNodeClaimIT[nodeClaimID] = storedCounterSetsByIT
	}

	// Compute old pessimistic max before merging new consumption.
	var oldCounterMax map[PoolKey]map[string]map[string]resourcev1.Counter
	if len(storedCounterSetsByIT) > 0 {
		oldCounterMax = pessimisticCounterMax(storedCounterSetsByIT)
	}

	// Merge new consumption into stored state.
	for it, counterSetsByPool := range newCounterConsumptionByIT {
		storedCounterSetsByPool, ok := storedCounterSetsByIT[it]
		if !ok {
			storedCounterSetsByIT[it] = counterSetsByPool
			continue
		}

		for poolKey, counterSets := range counterSetsByPool {
			storedCounterSets, ok := storedCounterSetsByPool[poolKey]
			if !ok {
				storedCounterSetsByPool[poolKey] = counterSets
				continue
			}
			for counterSetName, counters := range counterSets {
				storedCounterSet, ok := storedCounterSets[counterSetName]
				if !ok {
					storedCounterSets[counterSetName] = counters
					continue
				}
				for counterName, counter := range counters {
					storedCounter := storedCounterSet[counterName]
					storedCounter.Value.Add(counter.Value)
					storedCounterSet[counterName] = storedCounter
				}
			}
		}
	}

	// Compute new pessimistic max after merging.
	newCounterMax := pessimisticCounterMax(storedCounterSetsByIT)

	// Subtract only the delta (newMax - oldMax) from remaining counters.
	subtractDeltaFromRemaining(at.RemainingCounters, oldCounterMax, newCounterMax)
}

// commitTemplateCounters subtracts per-IT template counter consumption directly from the
// pre-initialized remaining budgets. Template counters don't need pessimistic-max treatment —
// each IT has its own independent budget.
func (at *AllocationTracker) commitTemplateCounters(nodeClaimID NodeClaimID, consumptionByIT map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter) {
	if len(consumptionByIT) == 0 {
		return
	}
	remainingCounterSetsByIT := at.templateRemainingCounters[nodeClaimID]
	for itID, counterSetsByPool := range consumptionByIT {
		remainingCounterSetsByPool := remainingCounterSetsByIT[itID]
		for poolKey, counterSets := range counterSetsByPool {
			remainingCounterSets := remainingCounterSetsByPool[poolKey]
			for counterSetName, counters := range counterSets {
				remainingCounterSet := remainingCounterSets[counterSetName]
				for counterName, counter := range counters {
					remainingCounter := remainingCounterSet[counterName]
					remainingCounter.Value.Sub(counter.Value)
					remainingCounterSet[counterName] = remainingCounter
				}
			}
		}
	}
}

// subtractDeltaFromRemaining subtracts (newCounterMax - oldCounterMax) from remaining counters.
func subtractDeltaFromRemaining(remaining map[PoolKey]map[string]map[string]resourcev1.Counter, oldCounterMax, newCounterMax map[PoolKey]map[string]map[string]resourcev1.Counter) {
	for poolKey, newCounterSets := range newCounterMax {
		poolRemaining, ok := remaining[poolKey]
		if !ok {
			continue
		}
		for counterSetName, newCounters := range newCounterSets {
			counterSetRemaining, ok := poolRemaining[counterSetName]
			if !ok {
				continue
			}
			for counterName, newCounter := range newCounters {
				delta := newCounter.Value.DeepCopy()
				if old, ok := getCounter(oldCounterMax, poolKey, counterSetName, counterName); ok {
					delta.Sub(old.Value)
				}
				if delta.Sign() > 0 {
					remainingCounter, ok := counterSetRemaining[counterName]
					if !ok {
						continue
					}
					remainingCounter.Value.Sub(delta)
					counterSetRemaining[counterName] = remainingCounter
				}
			}
		}
	}
}

func (at *AllocationTracker) ReleaseInstanceTypes(ctx context.Context, nodeClaim NodeClaimID, instanceTypes ...InstanceTypeID) {
	released := make(map[InstanceTypeID]sets.Set[DeviceID])
	for _, instanceType := range instanceTypes {
		devices := at.InflightClusterAllocationsByNodeClaim[nodeClaim][instanceType]
		released[instanceType] = devices
		delete(at.InflightClusterAllocationsByNodeClaim[nodeClaim], instanceType)
		for id := range devices {
			meta, ok := at.InflightClusterAllocations[id]
			if !ok {
				panic("missing reference count for device ID")
			}
			if !meta.InstanceTypes.Has(instanceType) {
				panic("inflight allocation metadata for device is missing instance type reference")
			}
			meta.InstanceTypes.Delete(instanceType)
			if len(meta.InstanceTypes) == 0 {
				delete(at.InflightClusterAllocations, id)
			}
		}
		delete(at.InflightTemplateAllocations[nodeClaim], instanceType)
	}
	at.releaseCounters(nodeClaim, instanceTypes)
	at.releaseTemplateCounters(nodeClaim, instanceTypes)

	if len(released) != 0 && log.FromContext(ctx).V(1).Enabled() {
		log.FromContext(ctx).V(1).Info("releasing allocations", "nodeClaimID", nodeClaim.Value(), "devicesByInstanceType", lo.MapEntries(released, func(it InstanceTypeID, ids sets.Set[DeviceID]) (string, []string) {
			return it.Value(), lo.Map(ids.UnsortedList(), func(id DeviceID, _ int) string { return id.String() })
		}))
	}
}

// releaseCounters adjusts remaining counters when instance types are pruned. Recomputes the
// pessimistic max from remaining ITs and adds back the delta.
func (at *AllocationTracker) releaseCounters(nodeClaimID NodeClaimID, releasedITs []InstanceTypeID) {
	storedCounterSetsByIT, ok := at.countersByNodeClaimIT[nodeClaimID]
	if !ok {
		return
	}

	oldCounterMax := pessimisticCounterMax(storedCounterSetsByIT)

	for _, itID := range releasedITs {
		delete(storedCounterSetsByIT, itID)
	}

	var newCounterMax map[PoolKey]map[string]map[string]resourcev1.Counter
	if len(storedCounterSetsByIT) > 0 {
		newCounterMax = pessimisticCounterMax(storedCounterSetsByIT)
	}
	addDeltaToRemaining(at.RemainingCounters, oldCounterMax, newCounterMax)

	if len(storedCounterSetsByIT) == 0 {
		delete(at.countersByNodeClaimIT, nodeClaimID)
	}
}

// releaseTemplateCounters removes template counter state for pruned instance types.
func (at *AllocationTracker) releaseTemplateCounters(nodeClaimID NodeClaimID, releasedITs []InstanceTypeID) {
	remainingCounterSetsByIT, ok := at.templateRemainingCounters[nodeClaimID]
	if !ok {
		return
	}
	for _, itID := range releasedITs {
		delete(remainingCounterSetsByIT, itID)
	}
	if len(remainingCounterSetsByIT) == 0 {
		delete(at.templateRemainingCounters, nodeClaimID)
	}
}

// addDeltaToRemaining adds (oldCounterMax - newCounterMax) back to remaining counters.
func addDeltaToRemaining(remaining map[PoolKey]map[string]map[string]resourcev1.Counter, oldCounterMax, newCounterMax map[PoolKey]map[string]map[string]resourcev1.Counter) {
	for poolKey, oldCounterSets := range oldCounterMax {
		poolRemaining, ok := remaining[poolKey]
		if !ok {
			continue
		}
		for counterSetName, oldCounters := range oldCounterSets {
			counterSetRemaining, ok := poolRemaining[counterSetName]
			if !ok {
				continue
			}
			for counterName, oldCounter := range oldCounters {
				delta := oldCounter.Value.DeepCopy()
				if new, ok := getCounter(newCounterMax, poolKey, counterSetName, counterName); ok {
					delta.Sub(new.Value)
				}
				if delta.Sign() > 0 {
					remainingCounter := counterSetRemaining[counterName]
					remainingCounter.Value.Add(delta)
					counterSetRemaining[counterName] = remainingCounter
				}
			}
		}
	}
}

func (at *AllocationTracker) IsAllocated(deviceID DeviceID, nodeClaim NodeClaim, instanceType InstanceTypeID) bool {
	if deviceID.Template {
		// Template devices are NodeClaim and instance type local. The device is only considered allocated if there's an entry
		// for the given NodeClaim-InstanceType combination.
		nodeClaimAllocs, ok := at.InflightTemplateAllocations[nodeClaim.ID()]
		if !ok {
			return false
		}
		instanceTypeAllocs, ok := nodeClaimAllocs[instanceType]
		if !ok {
			return false
		}
		if instanceTypeAllocs.Has(deviceID) {
			return true
		}
		return false
	}

	// The device is already marked as allocated on the cluster
	if at.PreallocatedDevices.Has(deviceID) {
		return true
	}
	if meta, ok := at.InflightClusterAllocations[deviceID]; ok {
		// If the device is marked as allocating for a different NodeClaim, we pessimistically assume it will be allocated
		if meta.NodeClaimID != nodeClaim.ID() {
			return true
		}
		// The device is marked as allocating for this NodeClaim and instance type combination so it can't be allocated again
		if meta.InstanceTypes.Has(instanceType) {
			return true
		}
		// The device is marked as allocating for this NodeClaim, but only for different instance types. Since the NodeClaim
		// will collapse to a single instance type, we can allocate it once per instance type.
		return false
	}
	// The device is neither marked as allocating in the cluster nor in the allocator's state, it's not allocated
	return false
}

// InitRemainingCounters initializes the remaining counter budget for a pool. Called lazily on first
// access for each pool during allocation. The initial value is the pool's total counter budget
// minus consumption from preallocated devices.
func (at *AllocationTracker) InitRemainingCounters(pool *Pool) {
	if _, ok := at.RemainingCounters[pool.Key]; ok {
		return
	}
	if len(pool.CounterSets) == 0 {
		return
	}
	remainingCounterSets := make(map[string]map[string]resourcev1.Counter, len(pool.CounterSets))
	for counterSetName, counters := range pool.CounterSets {
		remainingCounterSets[counterSetName] = make(map[string]resourcev1.Counter, len(counters))
		for counterName, counter := range counters {
			remainingCounterSets[counterSetName][counterName] = resourcev1.Counter{Value: counter.Value.DeepCopy()}
		}
	}
	// Deduct consumption from preallocated devices.
	for i := range pool.Devices {
		if !at.PreallocatedDevices.Has(pool.Devices[i].ID) {
			continue
		}
		deductFromCounters(remainingCounterSets, pool.Devices[i].Device)
	}
	for i := range pool.NonTargetingDevices {
		if !at.PreallocatedDevices.Has(pool.NonTargetingDevices[i].ID) {
			continue
		}
		deductFromCounters(remainingCounterSets, pool.NonTargetingDevices[i].Device)
	}
	at.RemainingCounters[pool.Key] = remainingCounterSets
}

// deductFromCounters subtracts a device's counter consumption from counter budgets.
func deductFromCounters(remainingCounterSets map[string]map[string]resourcev1.Counter, device cloudprovider.Device) {
	for _, consumption := range device.ConsumesCounters {
		counterSetRemaining, ok := remainingCounterSets[consumption.CounterSet]
		if !ok {
			continue
		}
		for counterName, counter := range consumption.Counters {
			remainingCounter, ok := counterSetRemaining[counterName]
			if !ok {
				continue
			}
			remainingCounter.Value.Sub(counter.Value)
			counterSetRemaining[counterName] = remainingCounter
		}
	}
}

// InitTemplateRemainingCounters lazily initializes the remaining counter budget for a
// (NodeClaim, IT) pair. The caller provides the total budget (computed from SharedCounters on
// the template slices). Subsequent calls for the same (NC, IT) are no-ops.
func (at *AllocationTracker) InitTemplateRemainingCounters(
	nodeClaimID NodeClaimID,
	itID InstanceTypeID,
	totals map[PoolKey]map[string]map[string]resourcev1.Counter,
) {
	remainingCounterSetsByIT, ok := at.templateRemainingCounters[nodeClaimID]
	if !ok {
		remainingCounterSetsByIT = make(map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter)
		at.templateRemainingCounters[nodeClaimID] = remainingCounterSetsByIT
	}
	if _, ok := remainingCounterSetsByIT[itID]; ok {
		return
	}
	remainingCounterSetsByIT[itID] = totals
}

// TemplateRemainingForIT returns the remaining template counter budget for the given
// (NodeClaim, IT) pair. Returns nil if not yet initialized.
func (at *AllocationTracker) TemplateRemainingForIT(nodeClaimID NodeClaimID, itID InstanceTypeID) map[PoolKey]map[string]map[string]resourcev1.Counter {
	remainingCounterSetsByIT, ok := at.templateRemainingCounters[nodeClaimID]
	if !ok {
		return nil
	}
	return remainingCounterSetsByIT[itID]
}

// pessimisticCounterMax computes the maximum counter value per pool/counterSet/counter across all ITs.
// returns map: poolKey → counterSetName → counterName → remaining counter.
func pessimisticCounterMax(counterConsumptionByIT map[InstanceTypeID]map[PoolKey]map[string]map[string]resourcev1.Counter) map[PoolKey]map[string]map[string]resourcev1.Counter {
	counterMaxByPool := make(map[PoolKey]map[string]map[string]resourcev1.Counter)
	for _, counterSetsByPool := range counterConsumptionByIT {
		for poolKey, counterSets := range counterSetsByPool {
			maxCounterSets, ok := counterMaxByPool[poolKey]
			if !ok {
				maxCounterSets = make(map[string]map[string]resourcev1.Counter)
				counterMaxByPool[poolKey] = maxCounterSets
			}
			for counterSetName, counters := range counterSets {
				maxCounters, ok := maxCounterSets[counterSetName]
				if !ok {
					maxCounters = make(map[string]resourcev1.Counter)
					maxCounterSets[counterSetName] = maxCounters
				}
				for counterName, counter := range counters {
					maxCounter, ok := maxCounters[counterName]
					if !ok || counter.Value.Cmp(maxCounter.Value) > 0 {
						maxCounters[counterName] = resourcev1.Counter{Value: counter.Value.DeepCopy()}
					}
				}
			}
		}
	}
	return counterMaxByPool
}

func getCounter(m map[PoolKey]map[string]map[string]resourcev1.Counter, pool PoolKey, set, name string) (resourcev1.Counter, bool) {
	if m == nil {
		return resourcev1.Counter{}, false
	}
	sets, ok := m[pool]
	if !ok {
		return resourcev1.Counter{}, false
	}
	counters, ok := sets[set]
	if !ok {
		return resourcev1.Counter{}, false
	}
	c, ok := counters[name]
	return c, ok
}
