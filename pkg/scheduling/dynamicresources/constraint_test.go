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

package dynamicresources_test

import (
	"unique"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

func device(name string, attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) cloudprovider.Device {
	return cloudprovider.Device{
		Name:       unique.Make(name),
		Attributes: attrs,
	}
}

var _ = Describe("Constraints", func() {
	Describe("MatchAttributeConstraint", func() {
		var (
			attrZone = resourcev1.QualifiedName("topology.kubernetes.io/zone")
			devA     = deviceID("driver.example.com", "pool-a", "dev-a")
			devB     = deviceID("driver.example.com", "pool-a", "dev-b")
			devC     = deviceID("driver.example.com", "pool-a", "dev-c")
		)

		deviceWithZone := func(name, zone string) cloudprovider.Device {
			return device(name, map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				attrZone: {StringValue: ptr.To(zone)},
			})
		}

		deviceWithoutZone := func(name string) cloudprovider.Device {
			return device(name, nil)
		}

		newConstraint := func(requestNames ...string) *dynamicresources.MatchAttributeConstraint {
			return &dynamicresources.MatchAttributeConstraint{
				RequestNames:  sets.New[string](requestNames...),
				AttributeName: attrZone,
			}
		}

		Describe("direct comparison (concrete attribute values)", func() {
			It("should accept the first device and pin its value", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
			})

			It("should accept a second device with the same attribute value", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithZone("dev-b", "us-west-2a"), devB)).To(BeTrue())
			})

			It("should reject a device with a different attribute value", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithZone("dev-b", "us-east-1a"), devB)).To(BeFalse())
			})

			It("should reject a device missing the attribute", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeFalse())
			})

			It("should only apply to named requests when requestNames is set", func() {
				c := newConstraint("req-a")
				// req-b is not in the constraint's request set — should be accepted
				Expect(c.Add("req-b", deviceWithZone("dev-b", "us-east-1a"), devB)).To(BeTrue())
				// req-a should pin
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
			})

			It("should apply to all requests when requestNames is empty", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithZone("dev-b", "us-east-1a"), devB)).To(BeFalse())
			})
		})

		Describe("backtracking via Remove()", func() {
			It("should allow a different value after removing all devices", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				c.Remove("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)
				// Pin is cleared — a different zone should now be accepted
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-east-1a"), devA)).To(BeTrue())
			})

			It("should maintain the pin after removing only one of two devices", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithZone("dev-b", "us-west-2a"), devB)).To(BeTrue())
				c.Remove("req-b", deviceWithZone("dev-b", "us-west-2a"), devB)
				// Pin still holds from dev-a — different zone should be rejected
				Expect(c.Add("req-c", deviceWithZone("dev-c", "us-east-1a"), devC)).To(BeFalse())
				// Same zone should still be accepted
				Expect(c.Add("req-c", deviceWithZone("dev-c", "us-west-2a"), devC)).To(BeTrue())
			})

			It("should be a no-op for non-matching request names", func() {
				c := newConstraint("req-a")
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				// Remove for a request not in the constraint's set — should not affect state.
				c.Remove("req-b", deviceWithZone("dev-b", "us-west-2a"), devB)
				// Pin from req-a should still hold.
				Expect(c.Add("req-a", deviceWithZone("dev-b", "us-east-1a"), devB)).To(BeFalse())
				Expect(c.Add("req-a", deviceWithZone("dev-b", "us-west-2a"), devB)).To(BeTrue())
			})
		})

		Describe("attribute binding fallback", func() {
			var (
				bindings dynamicresources.AttributeBindings
				itID     = unique.Make("gpu-xl")
			)

			BeforeEach(func() {
				bindings = dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
					"pool-a": {itType("gpu-xl",
						attrBinding(attrZone, devA, devB),
					)},
				})
			})

			newConstraintWithBindings := func() *dynamicresources.MatchAttributeConstraint {
				return &dynamicresources.MatchAttributeConstraint{
					RequestNames:  sets.New[string](),
					AttributeName: attrZone,
					AttributeBindingFallback: &dynamicresources.AttributeBindingFallback{
						Bindings:       bindings,
						NodePool:       "pool-a",
						InstanceTypeID: itID,
					},
				}
			}

			It("should accept two bound devices that both lack the attribute", func() {
				c := newConstraintWithBindings()
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithoutZone("dev-b"), devB)).To(BeTrue())
			})

			It("should reject the first device if it has no binding entries", func() {
				c := newConstraintWithBindings()
				// devC is not in any binding group for attrZone.
				Expect(c.Add("req-c", deviceWithoutZone("dev-c"), devC)).To(BeFalse())
			})

			It("should reject unbound devices that lack the attribute", func() {
				c := newConstraintWithBindings()
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeTrue())
				Expect(c.Add("req-c", deviceWithoutZone("dev-c"), devC)).To(BeFalse())
			})

			It("should not use bindings when concrete values are present", func() {
				c := newConstraintWithBindings()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithZone("dev-b", "us-east-1a"), devB)).To(BeFalse())
			})

			It("should reject concrete values after binding was established", func() {
				c := newConstraintWithBindings()
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithZone("dev-b", "us-west-2a"), devB)).To(BeFalse())
			})

			It("should reject binding fallback after concrete value was established", func() {
				c := newConstraintWithBindings()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithoutZone("dev-b"), devB)).To(BeFalse())
			})

			It("should reset evaluation path on full backtrack", func() {
				c := newConstraintWithBindings()
				// Establish via binding.
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeTrue())
				c.Remove("req-a", deviceWithoutZone("dev-a"), devA)
				// After full backtrack, concrete path should be accepted.
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
			})

			It("should accept a third bound device via transitivity", func() {
				// Build bindings with A-B and B-C declared separately.
				threeWayBindings := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
					"pool-a": {itType("gpu-xl",
						attrBinding(attrZone, devA, devB),
						attrBinding(attrZone, devB, devC),
					)},
				})
				c := &dynamicresources.MatchAttributeConstraint{
					RequestNames:  sets.New[string](),
					AttributeName: attrZone,
					AttributeBindingFallback: &dynamicresources.AttributeBindingFallback{
						Bindings:       threeWayBindings,
						NodePool:       "pool-a",
						InstanceTypeID: itID,
					},
				}
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithoutZone("dev-b"), devB)).To(BeTrue())
				// devC is transitively bound to devA via devB.
				Expect(c.Add("req-c", deviceWithoutZone("dev-c"), devC)).To(BeTrue())
			})

			It("should allow re-establishing via binding after full backtrack from binding", func() {
				c := newConstraintWithBindings()
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeTrue())
				c.Remove("req-a", deviceWithoutZone("dev-a"), devA)
				// Full backtrack — re-enter via binding path.
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithoutZone("dev-b"), devB)).To(BeTrue())
			})

			It("should clear UsedBinding flag on Reset allowing switch to concrete path", func() {
				c := newConstraintWithBindings()
				Expect(c.Add("req-a", deviceWithoutZone("dev-a"), devA)).To(BeTrue())
				Expect(c.UsedBinding).To(BeTrue())
				c.Reset()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
			})
		})

		Describe("Reset()", func() {
			It("should clear pinned attribute value and allow re-pinning to a different value", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				c.Reset()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-east-1a"), devA)).To(BeTrue())
			})

			It("should clear all mutable state", func() {
				c := newConstraint()
				Expect(c.Add("req-a", deviceWithZone("dev-a", "us-west-2a"), devA)).To(BeTrue())
				Expect(c.Add("req-b", deviceWithZone("dev-b", "us-west-2a"), devB)).To(BeTrue())
				c.Reset()
				Expect(c.AllocatedDeviceIDs).To(BeNil())
				Expect(c.AttributeValue).To(BeNil())
				Expect(c.UsedBinding).To(BeFalse())
			})
		})

		Describe("LookupAttribute", func() {
			It("should find a fully qualified attribute", func() {
				d := device("dev-a", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					"nvidia.com/model": {StringValue: ptr.To("H100")},
				})
				attr := dynamicresources.LookupAttribute(d, deviceID("nvidia.com", "pool", "dev"), "nvidia.com/model")
				Expect(attr).ToNot(BeNil())
				Expect(*attr.StringValue).To(Equal("H100"))
			})

			It("should fall back to driver-qualified lookup", func() {
				d := device("dev-a", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					"model": {StringValue: ptr.To("H100")},
				})
				attr := dynamicresources.LookupAttribute(d, deviceID("nvidia.com", "pool", "dev"), "nvidia.com/model")
				Expect(attr).ToNot(BeNil())
				Expect(*attr.StringValue).To(Equal("H100"))
			})

			It("should return nil when attribute is not found", func() {
				d := device("dev-a", nil)
				attr := dynamicresources.LookupAttribute(d, deviceID("nvidia.com", "pool", "dev"), "nvidia.com/model")
				Expect(attr).To(BeNil())
			})

			It("should return nil when attribute name has no domain separator", func() {
				d := device("dev-a", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					"model": {StringValue: ptr.To("H100")},
				})
				// "memory" has no "/" — no fallback attempted, and direct lookup misses.
				attr := dynamicresources.LookupAttribute(d, deviceID("nvidia.com", "pool", "dev"), "memory")
				Expect(attr).To(BeNil())
			})

			It("should not fall back when domain prefix does not match driver", func() {
				d := device("dev-a", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					"model": {StringValue: ptr.To("H100")},
				})
				// Domain "other.com" != driver "nvidia.com" — fallback skipped.
				attr := dynamicresources.LookupAttribute(d, deviceID("nvidia.com", "pool", "dev"), "other.com/model")
				Expect(attr).To(BeNil())
			})
		})

		Describe("AttributeValuesEqual", func() {
			It("should compare string values", func() {
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{StringValue: ptr.To("a")},
					&resourcev1.DeviceAttribute{StringValue: ptr.To("a")},
				)).To(BeTrue())
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{StringValue: ptr.To("a")},
					&resourcev1.DeviceAttribute{StringValue: ptr.To("b")},
				)).To(BeFalse())
			})

			It("should compare int values", func() {
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{IntValue: ptr.To(int64(42))},
					&resourcev1.DeviceAttribute{IntValue: ptr.To(int64(42))},
				)).To(BeTrue())
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{IntValue: ptr.To(int64(42))},
					&resourcev1.DeviceAttribute{IntValue: ptr.To(int64(43))},
				)).To(BeFalse())
			})

			It("should compare bool values", func() {
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{BoolValue: ptr.To(true)},
					&resourcev1.DeviceAttribute{BoolValue: ptr.To(true)},
				)).To(BeTrue())
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{BoolValue: ptr.To(true)},
					&resourcev1.DeviceAttribute{BoolValue: ptr.To(false)},
				)).To(BeFalse())
			})

			It("should compare version values", func() {
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{VersionValue: ptr.To("1.2.3")},
					&resourcev1.DeviceAttribute{VersionValue: ptr.To("1.2.3")},
				)).To(BeTrue())
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{VersionValue: ptr.To("1.2.3")},
					&resourcev1.DeviceAttribute{VersionValue: ptr.To("1.2.4")},
				)).To(BeFalse())
			})

			It("should return false for mismatched types", func() {
				Expect(dynamicresources.AttributeValuesEqual(
					&resourcev1.DeviceAttribute{StringValue: ptr.To("a")},
					&resourcev1.DeviceAttribute{IntValue: ptr.To(int64(1))},
				)).To(BeFalse())
			})

			It("should return false for nil values", func() {
				Expect(dynamicresources.AttributeValuesEqual(nil, nil)).To(BeFalse())
			})
		})
	})
})
