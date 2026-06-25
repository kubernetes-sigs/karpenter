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

	"github.com/imdario/mergo"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
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
