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
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

var _ = Describe("ResourceSlice Interface", func() {
	Describe("APIServerSlice", func() {
		var (
			apiSlice *resourcev1.ResourceSlice
			slice    dynamicresources.ResourceSlice
		)

		BeforeEach(func() {
			apiSlice = &resourcev1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-slice",
				},
				Spec: resourcev1.ResourceSliceSpec{
					Driver: "gpu.nvidia.com",
					Pool: resourcev1.ResourcePool{
						Name: "test-pool",
					},
					NodeName: ptr.To("test-node"),
					Devices: []resourcev1.Device{
						{
							Name: "gpu-0",
							Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.nvidia.com/model":  {StringValue: ptr.To("H100")},
								"gpu.nvidia.com/memory": {IntValue: ptr.To(int64(80))},
							},
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.nvidia.com/vram": {Value: resource.MustParse("80Gi")},
							},
							AllowMultipleAllocations: ptr.To(true),
						},
						{
							Name: "gpu-1",
							Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.nvidia.com/model":  {StringValue: ptr.To("H100")},
								"gpu.nvidia.com/memory": {IntValue: ptr.To(int64(80))},
							},
							AllowMultipleAllocations: nil,
						},
					},
				},
			}
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
		})

		It("should return the correct driver", func() {
			Expect(slice.Driver()).To(Equal(unique.Make("gpu.nvidia.com")))
		})

		It("should return the correct pool", func() {
			Expect(slice.Pool().Name).To(Equal(unique.Make("test-pool")))
		})

		It("should return Potential() as false", func() {
			Expect(slice.Potential()).To(BeFalse())
		})

		It("should convert devices with interned names, attributes, capacity, and AllowMultipleAllocations", func() {
			devices := slice.Devices()
			Expect(devices).To(HaveLen(2))
			Expect(devices[0].Name).To(Equal(unique.Make("gpu-0")))
			Expect(devices[0].Attributes).To(HaveLen(2))
			Expect(devices[0].Attributes["gpu.nvidia.com/model"].StringValue).To(Equal(ptr.To("H100")))
			Expect(devices[0].Attributes["gpu.nvidia.com/memory"].IntValue).To(Equal(ptr.To(int64(80))))
			Expect(devices[0].Capacity).To(HaveLen(1))
			Expect(devices[0].Capacity["gpu.nvidia.com/vram"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())
			Expect(devices[0].AllowMultipleAllocations).To(BeTrue())
			Expect(devices[1].Name).To(Equal(unique.Make("gpu-1")))
			Expect(devices[1].Capacity).To(BeEmpty())
			Expect(devices[1].AllowMultipleAllocations).To(BeFalse())
		})

		It("should cache devices on repeated calls", func() {
			devices1 := slice.Devices()
			devices2 := slice.Devices()
			// Pointer equality — same underlying slice returned
			Expect(&devices1[0]).To(BeIdenticalTo(&devices2[0]))
		})

		It("should return NodeSelector from the API object", func() {
			apiSlice.Spec.NodeSelector = &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
					}},
				},
			}
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
			Expect(slice.NodeSelector()).ToNot(BeNil())
			Expect(slice.NodeSelector().NodeSelectorTerms).To(HaveLen(1))
		})

		It("should return nil NodeSelector when not set", func() {
			apiSlice.Spec.NodeSelector = nil
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
			Expect(slice.NodeSelector()).To(BeNil())
		})

		It("should return AllNodes from the API object", func() {
			apiSlice.Spec.AllNodes = ptr.To(true)
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
			Expect(slice.AllNodes()).To(BeTrue())
		})

		It("should return false for AllNodes when nil", func() {
			apiSlice.Spec.AllNodes = nil
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
			Expect(slice.AllNodes()).To(BeFalse())
		})

		It("should return false for AllNodes when explicitly false", func() {
			apiSlice.Spec.AllNodes = ptr.To(false)
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
			Expect(slice.AllNodes()).To(BeFalse())
		})

		It("should handle empty device list", func() {
			apiSlice.Spec.Devices = nil
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
			Expect(slice.Devices()).To(BeEmpty())
		})

		It("should convert devices with ConsumesCounters", func() {
			apiSlice.Spec.Devices = []resourcev1.Device{
				{
					Name: "mig-3g.40gb-0",
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
			}
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
			devices := slice.Devices()
			Expect(devices).To(HaveLen(1))
			Expect(devices[0].ConsumesCounters).To(HaveLen(1))
			Expect(devices[0].ConsumesCounters[0].CounterSet).To(Equal("gpu-slices"))
			Expect(devices[0].ConsumesCounters[0].Counters).To(HaveLen(2))
			Expect(devices[0].ConsumesCounters[0].Counters["memory"].Value.Equal(resource.MustParse("40Gi"))).To(BeTrue())
			Expect(devices[0].ConsumesCounters[0].Counters["compute-units"].Value.Equal(resource.MustParse("3"))).To(BeTrue())
		})

		It("should return SharedCounters from the API object", func() {
			apiSlice.Spec.SharedCounters = []resourcev1.CounterSet{
				{
					Name: "gpu-slices",
					Counters: map[string]resourcev1.Counter{
						"memory":        {Value: resource.MustParse("80Gi")},
						"compute-units": {Value: resource.MustParse("7")},
					},
				},
			}
			slice = dynamicresources.NewAPIServerSlice(apiSlice)
			counters := slice.SharedCounters()
			Expect(counters).To(HaveLen(1))
			Expect(counters[0].Name).To(Equal("gpu-slices"))
			Expect(counters[0].Counters).To(HaveLen(2))
			Expect(counters[0].Counters["memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())
		})

	})

	Describe("TemplateSlice", func() {
		var (
			template *cloudprovider.ResourceSliceTemplate
			slice    dynamicresources.ResourceSlice
		)

		BeforeEach(func() {
			template = &cloudprovider.ResourceSliceTemplate{
				Driver: unique.Make("gpu.nvidia.com"),
				Pool:   cloudprovider.ResourcePool{Name: unique.Make("gpus")},
				Devices: []cloudprovider.Device{
					{
						Name: unique.Make("gpu-0"),
						Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.nvidia.com/model": {StringValue: ptr.To("A100")},
						},
						Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
							"gpu.nvidia.com/memory": {Value: resource.MustParse("80Gi")},
						},
						AllowMultipleAllocations: true,
					},
				},
			}
			slice = dynamicresources.NewTemplateSlice(template)
		})

		It("should return the correct driver", func() {
			Expect(slice.Driver()).To(Equal(unique.Make("gpu.nvidia.com")))
		})

		It("should return the correct pool", func() {
			Expect(slice.Pool().Name).To(Equal(unique.Make("gpus")))
		})

		It("should return Potential() as true", func() {
			Expect(slice.Potential()).To(BeTrue())
		})

		It("should return devices with attributes, capacity, and AllowMultipleAllocations", func() {
			devices := slice.Devices()
			Expect(devices).To(HaveLen(1))
			Expect(devices[0].Name).To(Equal(unique.Make("gpu-0")))
			Expect(devices[0].Attributes["gpu.nvidia.com/model"].StringValue).To(Equal(ptr.To("A100")))
			Expect(devices[0].Capacity).To(HaveLen(1))
			Expect(devices[0].Capacity["gpu.nvidia.com/memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())
			Expect(devices[0].AllowMultipleAllocations).To(BeTrue())
		})

		It("should return nil NodeSelector", func() {
			Expect(slice.NodeSelector()).To(BeNil())
		})

		It("should return false for AllNodes", func() {
			Expect(slice.AllNodes()).To(BeFalse())
		})

		It("should return SharedCounters from the template", func() {
			template.SharedCounters = []resourcev1.CounterSet{
				{
					Name: "gpu-slices",
					Counters: map[string]resourcev1.Counter{
						"memory":        {Value: resource.MustParse("80Gi")},
						"compute-units": {Value: resource.MustParse("7")},
					},
				},
			}
			slice = dynamicresources.NewTemplateSlice(template)
			counters := slice.SharedCounters()
			Expect(counters).To(HaveLen(1))
			Expect(counters[0].Name).To(Equal("gpu-slices"))
			Expect(counters[0].Counters).To(HaveLen(2))
			Expect(counters[0].Counters["memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())
		})

		It("should return devices with ConsumesCounters", func() {
			template.Devices = []cloudprovider.Device{
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
			}
			slice = dynamicresources.NewTemplateSlice(template)
			devices := slice.Devices()
			Expect(devices).To(HaveLen(1))
			Expect(devices[0].ConsumesCounters).To(HaveLen(1))
			Expect(devices[0].ConsumesCounters[0].CounterSet).To(Equal("gpu-slices"))
			Expect(devices[0].ConsumesCounters[0].Counters["memory"].Value.Equal(resource.MustParse("40Gi"))).To(BeTrue())
			Expect(devices[0].ConsumesCounters[0].Counters["compute-units"].Value.Equal(resource.MustParse("3"))).To(BeTrue())
		})
	})
})
