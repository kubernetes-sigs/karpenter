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

package test

import (
	"fmt"
	"strings"

	"github.com/imdario/mergo"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeviceClass creates a test DeviceClass with defaults that can be overridden by overrides.
// Overrides are applied in order, with a last write wins semantic.
func DeviceClass(overrides ...resourcev1.DeviceClass) *resourcev1.DeviceClass {
	override := resourcev1.DeviceClass{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	return &resourcev1.DeviceClass{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
	}
}

// DeviceClassWithSelector creates a DeviceClass whose single CEL selector matches the given driver.
func DeviceClassWithSelector(name, driver string) *resourcev1.DeviceClass {
	return DeviceClass(resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.DeviceClassSpec{
			Selectors: []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: fmt.Sprintf("device.driver == %q", driver)}},
			},
		},
	})
}

// ResourceClaim creates a test ResourceClaim with defaults that can be overridden by overrides.
// Overrides are applied in order, with a last write wins semantic.
func ResourceClaim(overrides ...resourcev1.ResourceClaim) *resourcev1.ResourceClaim {
	override := resourcev1.ResourceClaim{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	if override.Namespace == "" {
		override.Namespace = "default"
	}
	return &resourcev1.ResourceClaim{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
		Status:     override.Status,
	}
}

// ResourceClaimTemplate creates a test ResourceClaimTemplate with defaults that can be overridden by overrides.
// Overrides are applied in order, with a last write wins semantic.
func ResourceClaimTemplate(overrides ...resourcev1.ResourceClaimTemplate) *resourcev1.ResourceClaimTemplate {
	override := resourcev1.ResourceClaimTemplate{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	if override.Namespace == "" {
		override.Namespace = "default"
	}
	return &resourcev1.ResourceClaimTemplate{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
	}
}

// ExactDeviceRequest builds a DeviceRequest for a fixed number of devices of the given class.
func ExactDeviceRequest(name, className string, count int64) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: className,
			Count:           count,
		},
	}
}

// ExactDeviceRequestWithCapacity builds a DeviceRequest for a fixed number of devices of the given class that also
// requests a slice of per-dimension capacity from each device (the consumable-capacity path). A nil or empty capacity
// map is equivalent to ExactDeviceRequest.
func ExactDeviceRequestWithCapacity(name, className string, count int64, capacity map[resourcev1.QualifiedName]resource.Quantity) resourcev1.DeviceRequest {
	req := ExactDeviceRequest(name, className, count)
	if len(capacity) > 0 {
		req.Exactly.Capacity = &resourcev1.CapacityRequirements{Requests: capacity}
	}
	return req
}

// AllDeviceRequest builds a DeviceRequest in "All" allocation mode for the given class.
func AllDeviceRequest(name, className string) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: className,
			AllocationMode:  resourcev1.DeviceAllocationModeAll,
		},
	}
}

// MatchAttributeConstraint builds a DeviceConstraint requiring all listed requests to share an attribute value.
// If requests is empty, the constraint applies to all requests in the claim.
func MatchAttributeConstraint(attribute string, requests ...string) resourcev1.DeviceConstraint {
	return resourcev1.DeviceConstraint{
		Requests:       requests,
		MatchAttribute: lo.ToPtr(resourcev1.FullyQualifiedName(attribute)),
	}
}

// ResourceClaimForRequests builds a ResourceClaim in the default namespace containing the given device requests.
func ResourceClaimForRequests(name string, requests ...resourcev1.DeviceRequest) *resourcev1.ResourceClaim {
	return ResourceClaim(resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: requests},
		},
	})
}

// ResourceSlice creates a test ResourceSlice with defaults that can be overridden by overrides.
// Overrides are applied in order, with a last write wins semantic. This is intended for cluster-wide
// or non-node-local slices that the test manages directly; node-local slices for provisioned nodes are
// created automatically by ExpectProvisioned.
func ResourceSlice(overrides ...resourcev1.ResourceSlice) *resourcev1.ResourceSlice {
	override := resourcev1.ResourceSlice{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	if override.Spec.Pool.Name == "" {
		override.Spec.Pool.Name = override.Name
	}
	if override.Spec.Pool.ResourceSliceCount == 0 {
		override.Spec.Pool.ResourceSliceCount = 1
	}
	if override.Spec.Pool.Generation == 0 {
		override.Spec.Pool.Generation = 1
	}
	// A slice must be node-local, cluster-wide, or use a node selector. Default to cluster-wide when unset so the
	// slice is usable from any node.
	if override.Spec.NodeName == nil && override.Spec.NodeSelector == nil && override.Spec.AllNodes == nil {
		override.Spec.AllNodes = lo.ToPtr(true)
	}
	return &resourcev1.ResourceSlice{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
	}
}

// Device builds a ResourceSlice device with the given name and optional attributes.
func Device(name string, attributes map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) resourcev1.Device {
	return resourcev1.Device{
		Name:       name,
		Attributes: attributes,
	}
}

// StringAttribute is a convenience for building a string-valued device attribute.
func StringAttribute(value string) resourcev1.DeviceAttribute {
	return resourcev1.DeviceAttribute{StringValue: lo.ToPtr(value)}
}

// PodResourceClaimReference builds a PodResourceClaim referencing an existing ResourceClaim by name.
func PodResourceClaimReference(name, claimName string) corev1.PodResourceClaim {
	return corev1.PodResourceClaim{Name: name, ResourceClaimName: lo.ToPtr(claimName)}
}

// PodResourceClaimTemplateReference builds a PodResourceClaim referencing a ResourceClaimTemplate by name.
func PodResourceClaimTemplateReference(name, templateName string) corev1.PodResourceClaim {
	return corev1.PodResourceClaim{Name: name, ResourceClaimTemplateName: lo.ToPtr(templateName)}
}

const (
	// GPUDriver and NICDriver are the DRA driver names used by the shared DRA test fixtures.
	GPUDriver = "gpu.example.com"
	NICDriver = "nic.example.com"
	// CapacityMemory is the consumable-capacity dimension used by the shared DRA test fixtures.
	CapacityMemory resourcev1.QualifiedName = GPUDriver + "/memory"
)

// NodeLocalPoolName returns the auto-generated pool name for a node-local DRA driver's ResourceSlice, matching the
// framework's <driver>/<node> convention so device allocation status reconciliation lines up.
func NodeLocalPoolName(driverName, nodeName string) string {
	return fmt.Sprintf("%s/%s", driverName, nodeName)
}

// SanitizeDriver renders a driver name safe for use in object names by replacing "." and "/" with "-".
func SanitizeDriver(driver string) string {
	return strings.ReplaceAll(strings.ReplaceAll(driver, ".", "-"), "/", "-")
}

// CapacityRequest builds a single-dimension (CapacityMemory) capacity request map for the given quantity.
func CapacityRequest(amount string) map[resourcev1.QualifiedName]resource.Quantity {
	return map[resourcev1.QualifiedName]resource.Quantity{CapacityMemory: resource.MustParse(amount)}
}

// NodeLocalSlice builds a published in-cluster ResourceSlice owned by and pinned to the given node via spec.nodeName —
// the form kubelet/DRA drivers use for node-local devices. Such a slice is accessible only from the existing node with
// that name. The pool name follows the NodeLocalPoolName convention so device allocation status reconciliation lines up.
func NodeLocalSlice(node *corev1.Node, driver string, deviceNames ...string) *resourcev1.ResourceSlice {
	return ResourceSlice(resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", node.Name, SanitizeDriver(driver)),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       node.Name,
				UID:        node.UID,
			}},
		},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			NodeName: lo.ToPtr(node.Name),
			Pool:     resourcev1.ResourcePool{Name: NodeLocalPoolName(driver, node.Name), Generation: 1, ResourceSliceCount: 1},
			Devices:  lo.Map(deviceNames, func(name string, _ int) resourcev1.Device { return resourcev1.Device{Name: name} }),
		},
	})
}

// ZonedSlice builds a published, cluster-managed ResourceSlice whose devices are constrained to a topology zone via a
// NodeSelector. It is not owned by any node (cluster-managed), so it is always gathered, and its zone requirement is
// propagated onto a NodeClaim that allocates from it.
func ZonedSlice(name, driver, zone string, deviceNames ...string) *resourcev1.ResourceSlice {
	return ResourceSlice(resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver: driver,
			Pool:   resourcev1.ResourcePool{Name: name, Generation: 1, ResourceSliceCount: 1},
			NodeSelector: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      corev1.LabelTopologyZone,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{zone},
				}},
			}}},
			Devices: lo.Map(deviceNames, func(name string, _ int) resourcev1.Device { return resourcev1.Device{Name: name} }),
		},
	})
}

// ClusterWideSlice builds a published, cluster-wide (AllNodes) ResourceSlice. Its devices are accessible from any node
// and the slice is always gathered (no node owner reference), isolating device-availability behavior from node state.
func ClusterWideSlice(name, driver string, deviceNames ...string) *resourcev1.ResourceSlice {
	return ResourceSlice(resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			AllNodes: lo.ToPtr(true),
			Pool:     resourcev1.ResourcePool{Name: name, Generation: 1, ResourceSliceCount: 1},
			Devices:  lo.Map(deviceNames, func(n string, _ int) resourcev1.Device { return resourcev1.Device{Name: n} }),
		},
	})
}

// SharedCapacitySlice builds a published, cluster-wide (AllNodes) ResourceSlice whose single device is
// multi-allocatable and publishes the given total memory capacity. Used to seed an in-cluster shared device for the
// consumable-capacity tests.
func SharedCapacitySlice(name, driver, device, totalMemory string) *resourcev1.ResourceSlice {
	return ResourceSlice(resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			AllNodes: lo.ToPtr(true),
			Pool:     resourcev1.ResourcePool{Name: name, Generation: 1, ResourceSliceCount: 1},
			Devices: []resourcev1.Device{{
				Name:                     device,
				AllowMultipleAllocations: lo.ToPtr(true),
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					CapacityMemory: {Value: resource.MustParse(totalMemory)},
				},
			}},
		},
	})
}

// AllocatedClusterWideClaim builds a ResourceClaim already allocated to a cluster-wide published device, reserved for
// the given consumers. This seeds the deviceallocation controller's tracking so the allocator treats it as in-use.
func AllocatedClusterWideClaim(name, pool, driver, device string, consumers ...resourcev1.ResourceClaimConsumerReference) *resourcev1.ResourceClaim {
	return ResourceClaim(resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{ExactDeviceRequest("req", "gpu", 1)}},
		},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{{
						Request: "req",
						Driver:  driver,
						Pool:    pool,
						Device:  device,
					}},
				},
			},
			ReservedFor: consumers,
		},
	})
}

// AllocatedSharedClaim builds a ResourceClaim already allocated to a shared (multi-allocatable) device, consuming the
// given capacity and reserved for the given consumers. This seeds the deviceallocation controller so the allocator sees
// the device's consumed capacity (and, via per-claim contributions, which pods hold which share).
func AllocatedSharedClaim(name, pool, driver, device string, consumed map[resourcev1.QualifiedName]resource.Quantity, consumers ...resourcev1.ResourceClaimConsumerReference) *resourcev1.ResourceClaim {
	return ResourceClaim(resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
				ExactDeviceRequestWithCapacity("req", "gpu", 1, consumed),
			}},
		},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{{
						Request:          "req",
						Driver:           driver,
						Pool:             pool,
						Device:           device,
						ConsumedCapacity: consumed,
					}},
				},
			},
			ReservedFor: consumers,
		},
	})
}

// PodConsumer builds a ResourceClaimConsumerReference for a pod, for use as a ResourceClaim's reservedFor entry.
func PodConsumer(pod *corev1.Pod) resourcev1.ResourceClaimConsumerReference {
	return resourcev1.ResourceClaimConsumerReference{Resource: "pods", Name: pod.Name, UID: pod.UID}
}
