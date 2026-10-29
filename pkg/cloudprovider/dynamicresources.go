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

package cloudprovider

import (
	"unique"

	resourcev1 "k8s.io/api/resource/v1"

	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// DynamicResources is one DRA driver's device set for an instance type. Cloud providers populate these to enable
// DRA-aware scheduling simulation. A set is published by the instance type's offerings that are compatible with its
// Requirements, so mutually exclusive launch configurations (e.g. DRA vs. device plugin) can expose different devices.
//
// Cloud providers are expected to uphold the following conventions. Behavior is undefined if they're violated.
//  1. Every template in a set has the same driver.
//  2. For each driver, every offering is compatible with the requirements of at most one of that driver's sets.
//  3. Every set is compatible with at least one of the instance type's offerings.
//  4. A device (driver, pool, device name) appears in at most one of an instance type's sets.
//  5. Attribute bindings hold in every launch configuration in which the bound devices coexist.
//  6. Every key a set's requirements reference is an offering-level, well-known label. The instance type's
//     requirements define it as In the union of its offerings' values, or as DoesNotExist if no offering carries it.
//  7. Each offering either omits a set key or carries it as a single In value, and within an instance type, offerings
//     either all carry a given set key or all omit it. An offering carrying several values (e.g. both modes) would stay
//     compatible with a set after the NodeClaim narrows to it, and its single CapacityOverride couldn't describe both
//     modes. An offering that omits a key is compatible with every set on that key.
//  8. Offerings that differ only in set keys have the same price, since consolidation prices a candidate without
//     knowing which set it launched with.
type DynamicResources struct {
	// Requirements selects the offerings that publish this set. It may only reference offering-level, well-known keys,
	// which the instance type's requirements must define. If empty, every offering publishes this set.
	Requirements scheduling.Requirements
	// ResourceSliceTemplates describes the DRA devices in this set. Each entry is an in-memory template for a
	// ResourceSlice that the corresponding DRA driver is expected to publish once the node is running.
	ResourceSliceTemplates []*ResourceSliceTemplate
}

// ResourceSliceTemplate is the cloud provider's in-memory representation of a ResourceSlice.
// It only describes node-local devices — node-affinity fields are omitted since all
// cloud-provider-supplied templates are implicitly node-local.
type ResourceSliceTemplate struct {
	// Driver is the DRA driver name (e.g., "gpu.nvidia.com").
	Driver unique.Handle[string]
	// Pool identifies the device pool this slice belongs to.
	Pool ResourcePool
	// Devices is the list of expected devices in this slice.
	Devices []Device
	// SharedCounters is the list of expected counter sets in this slice.
	// A ResourceSliceTemplate with SharedCounters must not also contain Devices —
	// these are mutually exclusive. If both are set, the devices will be silently
	// ignored during pool gathering.
	SharedCounters []resourcev1.CounterSet
}

// ResourcePool identifies a pool of devices within a driver.
type ResourcePool struct {
	Name unique.Handle[string]
}

// Device describes a single device within a ResourceSlice, with attributes for CEL
// selector evaluation and MatchAttribute constraint checking.
type Device struct {
	// Name uniquely identifies this device within its pool.
	Name unique.Handle[string]
	// Attributes are the device's properties used for CEL selector evaluation and
	// MatchAttribute constraint checking.
	Attributes map[resourcev1.QualifiedName]resourcev1.DeviceAttribute
	// Capacity defines the set of capacities for this device.
	Capacity map[resourcev1.QualifiedName]resourcev1.DeviceCapacity
	// AllowMultipleAllocations marks whether the device is allowed to be allocated
	// to multiple DeviceRequests.
	AllowMultipleAllocations bool
	// ConsumesCounters declares which sharedCounters this device consumes from
	// on allocation.
	ConsumesCounters []resourcev1.DeviceCounterConsumption
}

// AttributeBinding declares that a set of devices on an instance type will share a common
// value for a given attribute at runtime. This enables the allocator to satisfy MatchAttribute
// constraints for runtime-only attributes that cannot be included in device templates.
type AttributeBinding struct {
	// Devices is the set of devices that share the attribute value. Must contain
	// at least 2 devices; bindings with fewer are ignored.
	Devices []DeviceID
	// Attribute is the fully qualified attribute name that the devices share.
	Attribute resourcev1.QualifiedName
}
