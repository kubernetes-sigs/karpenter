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

package cloudprovider_test

import (
	"unique"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var _ = Describe("DynamicResources", func() {
	Describe("DeepCopy", func() {
		It("should deep copy ResourceSliceTemplates and AttributeBindings", func() {
			original := cloudprovider.DynamicResources{
				ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{
					{
						Driver: unique.Make("gpu.nvidia.com"),
						Pool:   cloudprovider.ResourcePool{Name: unique.Make("gpus")},
						Devices: []cloudprovider.Device{
							{
								Name: unique.Make("gpu-0"),
								Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
									"gpu.nvidia.com/model": {StringValue: ptr.To("H100")},
								},
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.nvidia.com/memory": {Value: resource.MustParse("80Gi")},
								},
								AllowMultipleAllocations: true,
							},
						},
					},
				},
				AttributeBindings: []*cloudprovider.AttributeBinding{
					{
						Attribute: "topology.nvidia.com/pcie-root",
						Devices: []cloudprovider.DeviceID{
							{Driver: unique.Make("gpu.nvidia.com"), Pool: unique.Make("gpus"), Device: unique.Make("gpu-0")},
							{Driver: unique.Make("nic.aws.com"), Pool: unique.Make("nics"), Device: unique.Make("nic-0")},
						},
					},
				},
			}

			copied := original.DeepCopy()

			Expect(copied.ResourceSliceTemplates).To(HaveLen(1))
			Expect(copied.ResourceSliceTemplates[0].Driver).To(Equal(unique.Make("gpu.nvidia.com")))
			Expect(copied.ResourceSliceTemplates[0].Devices).To(HaveLen(1))
			Expect(copied.AttributeBindings).To(HaveLen(1))
			Expect(copied.AttributeBindings[0].Devices).To(HaveLen(2))

			dev := copied.ResourceSliceTemplates[0].Devices[0]
			Expect(dev.AllowMultipleAllocations).To(BeTrue())
			Expect(dev.Capacity).To(HaveLen(1))
			Expect(dev.Capacity["gpu.nvidia.com/memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())

			// Mutating the copy should not affect the original
			copied.ResourceSliceTemplates[0].Devices[0].Attributes["gpu.nvidia.com/model"] = resourcev1.DeviceAttribute{
				StringValue: ptr.To("A100"),
			}
			Expect(*original.ResourceSliceTemplates[0].Devices[0].Attributes["gpu.nvidia.com/model"].StringValue).To(Equal("H100"))

			dev.Capacity["gpu.nvidia.com/memory"] = resourcev1.DeviceCapacity{Value: resource.MustParse("40Gi")}
			Expect(original.ResourceSliceTemplates[0].Devices[0].Capacity["gpu.nvidia.com/memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())

			copied.AttributeBindings[0].Devices = copied.AttributeBindings[0].Devices[:1]
			Expect(original.AttributeBindings[0].Devices).To(HaveLen(2))
		})

		It("should return nil when copying nil", func() {
			var dr *cloudprovider.DynamicResources
			Expect(dr.DeepCopy()).To(BeNil())
		})

		It("should handle zero value", func() {
			original := cloudprovider.DynamicResources{}
			copied := original.DeepCopy()
			Expect(copied.ResourceSliceTemplates).To(BeNil())
			Expect(copied.AttributeBindings).To(BeNil())
		})

		It("should deep copy SharedCounters and ConsumesCounters", func() {
			original := cloudprovider.DynamicResources{
				ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{
					{
						Driver: unique.Make("gpu.nvidia.com"),
						Pool:   cloudprovider.ResourcePool{Name: unique.Make("mig-pool")},
						SharedCounters: []resourcev1.CounterSet{
							{
								Name: "gpu-slices",
								Counters: map[string]resourcev1.Counter{
									"memory":        {Value: resource.MustParse("80Gi")},
									"compute-units": {Value: resource.MustParse("7")},
								},
							},
						},
						Devices: []cloudprovider.Device{
							{
								Name: unique.Make("mig-3g.40gb-0"),
								ConsumesCounters: []resourcev1.DeviceCounterConsumption{
									{
										CounterSet: "gpu-slices",
										Counters: map[string]resourcev1.Counter{
											"memory":        {Value: resource.MustParse("40Gi")},
											"compute-units": {Value: resource.MustParse("3")},
										},
									},
								},
							},
						},
					},
				},
			}

			copied := original.DeepCopy()

			// Verify structural correctness
			Expect(copied.ResourceSliceTemplates).To(HaveLen(1))
			tmpl := copied.ResourceSliceTemplates[0]
			Expect(tmpl.SharedCounters).To(HaveLen(1))
			Expect(tmpl.SharedCounters[0].Name).To(Equal("gpu-slices"))
			Expect(tmpl.SharedCounters[0].Counters).To(HaveLen(2))
			Expect(tmpl.Devices[0].ConsumesCounters).To(HaveLen(1))
			Expect(tmpl.Devices[0].ConsumesCounters[0].CounterSet).To(Equal("gpu-slices"))
			Expect(tmpl.Devices[0].ConsumesCounters[0].Counters).To(HaveLen(2))

			// Mutating SharedCounters on the copy should not affect the original
			tmpl.SharedCounters[0].Counters["memory"] = resourcev1.Counter{Value: resource.MustParse("10Gi")}
			Expect(original.ResourceSliceTemplates[0].SharedCounters[0].Counters["memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())

			// Mutating ConsumesCounters on the copy should not affect the original
			tmpl.Devices[0].ConsumesCounters[0].Counters["memory"] = resourcev1.Counter{Value: resource.MustParse("5Gi")}
			Expect(original.ResourceSliceTemplates[0].Devices[0].ConsumesCounters[0].Counters["memory"].Value.Equal(resource.MustParse("40Gi"))).To(BeTrue())
		})
	})
})
