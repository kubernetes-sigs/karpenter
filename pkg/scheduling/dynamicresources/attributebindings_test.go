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

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

func deviceID(driver, pool, device string) dynamicresources.DeviceID {
	return dynamicresources.DeviceID{DeviceID: cloudprovider.DeviceID{
		Driver: unique.Make(driver),
		Pool:   unique.Make(pool),
		Device: unique.Make(device),
	}}
}

func itType(name string, bindings ...*cloudprovider.AttributeBinding) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name: name,
		DynamicResources: cloudprovider.DynamicResources{
			AttributeBindings: bindings,
		},
	}
}

func attrBinding(attribute resourcev1.QualifiedName, devices ...dynamicresources.DeviceID) *cloudprovider.AttributeBinding {
	cpDevices := make([]cloudprovider.DeviceID, len(devices))
	for i, d := range devices {
		cpDevices[i] = d.DeviceID
	}
	return &cloudprovider.AttributeBinding{
		Attribute: attribute,
		Devices:   cpDevices,
	}
}

var _ = Describe("AttributeBindings", func() {
	var (
		devA = deviceID("driver.example.com", "pool-a", "device-a")
		devB = deviceID("driver.example.com", "pool-a", "device-b")
		devC = deviceID("driver.example.com", "pool-a", "device-c")
		devD = deviceID("driver.example.com", "pool-a", "device-d")

		attrZone   = resourcev1.QualifiedName("topology.kubernetes.io/zone")
		attrRegion = resourcev1.QualifiedName("topology.kubernetes.io/region")
	)

	Describe("BuildAttributeBindings", func() {
		It("returns empty bindings when given no instance types", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{})
			Expect(ab).To(BeEmpty())
		})
		It("returns empty bindings when instance types have no AttributeBindings", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl")},
			})
			Expect(ab).To(BeEmpty())
		})
		It("skips a binding with fewer than 2 devices", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone, devA))},
			})
			Expect(ab).To(BeEmpty())
		})
		It("skips a binding with zero devices", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone))},
			})
			Expect(ab).To(BeEmpty())
		})
		It("creates a symmetric pair from a 2-device binding", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone, devA, devB))},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devB, devA)).To(BeTrue())
		})
		It("creates all-pairs bindings from a 3-device binding", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone, devA, devB, devC))},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devA, devC)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devB, devA)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devB, devC)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devC, devA)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devC, devB)).To(BeTrue())
		})
		It("accumulates device maps when multiple bindings share the same attribute, node pool, and instance type", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl",
					attrBinding(attrZone, devA, devB),
					attrBinding(attrZone, devC, devD),
				)},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devC, devD)).To(BeTrue())
			// The two components are disjoint — devA is not bound to devC
			Expect(ab.Bound("pool-a", it, attrZone, devA, devC)).To(BeFalse())
		})
		It("binding is transitive across separate AttributeBinding declarations", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl",
					attrBinding(attrZone, devA, devB),
					attrBinding(attrZone, devB, devC),
				)},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devB, devA)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devB, devC)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devC, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devA, devC)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devC, devA)).To(BeTrue())
		})
		It("transitive closure extends beyond a single intermediate device", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl",
					attrBinding(attrZone, devA, devB),
					attrBinding(attrZone, devB, devC),
					attrBinding(attrZone, devC, devD),
				)},
			})
			it := unique.Make("gpu-xl")
			for _, pair := range [][2]dynamicresources.DeviceID{
				{devA, devB}, {devA, devC}, {devA, devD},
				{devB, devA}, {devB, devC}, {devB, devD},
				{devC, devA}, {devC, devB}, {devC, devD},
				{devD, devA}, {devD, devB}, {devD, devC},
			} {
				Expect(ab.Bound("pool-a", it, attrZone, pair[0], pair[1])).To(BeTrue())
			}
		})
		It("a duplicate device entry in a binding does not prevent real bindings", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone, devA, devA, devB))},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devA, devA)).To(BeTrue())
		})
		It("all-duplicate devices in a binding produces no real bindings", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone, devA, devA))},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.HasBindings("pool-a", it, attrZone, devA)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devA, devA)).To(BeFalse())
		})
		It("transitive closure handles cycles correctly", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl",
					attrBinding(attrZone, devA, devB),
					attrBinding(attrZone, devB, devC),
					attrBinding(attrZone, devC, devA),
				)},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devA, devC)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devB, devC)).To(BeTrue())
		})
		It("same device pair can be bound independently under multiple attributes", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl",
					attrBinding(attrZone, devA, devB),
					attrBinding(attrRegion, devA, devB),
				)},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrRegion, devA, devB)).To(BeTrue())
		})
		It("transitivity does not cross attribute boundaries", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl",
					attrBinding(attrZone, devA, devB),
					attrBinding(attrRegion, devB, devC),
				)},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devC)).To(BeFalse())
			Expect(ab.Bound("pool-a", it, attrRegion, devA, devC)).To(BeFalse())
		})
		It("tracks different attributes independently", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl",
					attrBinding(attrZone, devA, devB),
					attrBinding(attrRegion, devB, devC),
				)},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrRegion, devB, devC)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrRegion, devA, devB)).To(BeFalse())
		})
		It("tracks the same attribute across different node pools independently", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone, devA, devB))},
				"pool-b": {itType("gpu-xl", attrBinding(attrZone, devB, devC))},
			})
			it := unique.Make("gpu-xl")
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-b", it, attrZone, devB, devC)).To(BeTrue())
			Expect(ab.Bound("pool-b", it, attrZone, devA, devB)).To(BeFalse())
		})
		It("tracks the same attribute across multiple instance types in a pool independently", func() {
			ab := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {
					itType("gpu-sm", attrBinding(attrZone, devA, devB)),
					itType("gpu-xl", attrBinding(attrZone, devB, devC)),
				},
			})
			Expect(ab.Bound("pool-a", unique.Make("gpu-sm"), attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", unique.Make("gpu-xl"), attrZone, devB, devC)).To(BeTrue())
			Expect(ab.Bound("pool-a", unique.Make("gpu-xl"), attrZone, devA, devB)).To(BeFalse())
		})
	})

	Describe("Bound", func() {
		var (
			ab dynamicresources.AttributeBindings
			it = unique.Make("gpu-xl")
		)

		BeforeEach(func() {
			ab = dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone, devA, devB))},
			})
		})

		It("returns true for a fully known tuple", func() {
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
		})
		It("is symmetric: Bound(A, B) and Bound(B, A) are both true", func() {
			Expect(ab.Bound("pool-a", it, attrZone, devA, devB)).To(BeTrue())
			Expect(ab.Bound("pool-a", it, attrZone, devB, devA)).To(BeTrue())
		})
		It("returns false for an unknown attribute", func() {
			Expect(ab.Bound("pool-a", it, attrRegion, devA, devB)).To(BeFalse())
		})
		It("returns false for an unknown node pool", func() {
			Expect(ab.Bound("pool-b", it, attrZone, devA, devB)).To(BeFalse())
		})
		It("returns false for an unknown instance type", func() {
			Expect(ab.Bound("pool-a", unique.Make("gpu-sm"), attrZone, devA, devB)).To(BeFalse())
		})
		It("returns false for an unknown deviceA", func() {
			Expect(ab.Bound("pool-a", it, attrZone, devC, devB)).To(BeFalse())
		})
		It("returns false when deviceB is not in deviceA's binding set", func() {
			Expect(ab.Bound("pool-a", it, attrZone, devA, devC)).To(BeFalse())
		})
		It("returns true when deviceA and deviceB are the same device with bindings", func() {
			Expect(ab.Bound("pool-a", it, attrZone, devA, devA)).To(BeTrue())
		})
		It("returns false when deviceA and deviceB are the same device without bindings", func() {
			Expect(ab.Bound("pool-a", it, attrZone, devC, devC)).To(BeFalse())
		})
		It("returns false on a nil AttributeBindings", func() {
			var empty dynamicresources.AttributeBindings
			Expect(empty.Bound("pool-a", it, attrZone, devA, devB)).To(BeFalse())
		})
	})

	Describe("HasBindings", func() {
		var (
			ab dynamicresources.AttributeBindings
			it = unique.Make("gpu-xl")
		)

		BeforeEach(func() {
			ab = dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"pool-a": {itType("gpu-xl", attrBinding(attrZone, devA, devB))},
			})
		})

		It("returns true for a device that participates in a binding", func() {
			Expect(ab.HasBindings("pool-a", it, attrZone, devA)).To(BeTrue())
			Expect(ab.HasBindings("pool-a", it, attrZone, devB)).To(BeTrue())
		})
		It("returns false for a device not in any binding group", func() {
			Expect(ab.HasBindings("pool-a", it, attrZone, devC)).To(BeFalse())
		})
		It("returns false for an unknown attribute", func() {
			Expect(ab.HasBindings("pool-a", it, attrRegion, devA)).To(BeFalse())
		})
		It("returns false for an unknown node pool", func() {
			Expect(ab.HasBindings("pool-b", it, attrZone, devA)).To(BeFalse())
		})
		It("returns false for an unknown instance type", func() {
			Expect(ab.HasBindings("pool-a", unique.Make("gpu-sm"), attrZone, devA)).To(BeFalse())
		})
		It("returns false on a nil AttributeBindings", func() {
			var empty dynamicresources.AttributeBindings
			Expect(empty.HasBindings("pool-a", it, attrZone, devA)).To(BeFalse())
		})
	})
})
