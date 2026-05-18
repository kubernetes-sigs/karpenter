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
						},
						{
							Name: "gpu-1",
							Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.nvidia.com/model":  {StringValue: ptr.To("H100")},
								"gpu.nvidia.com/memory": {IntValue: ptr.To(int64(80))},
							},
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

		It("should convert devices with interned names and attributes", func() {
			devices := slice.Devices()
			Expect(devices).To(HaveLen(2))
			Expect(devices[0].Name).To(Equal(unique.Make("gpu-0")))
			Expect(devices[0].Attributes).To(HaveLen(2))
			Expect(devices[0].Attributes["gpu.nvidia.com/model"].StringValue).To(Equal(ptr.To("H100")))
			Expect(devices[0].Attributes["gpu.nvidia.com/memory"].IntValue).To(Equal(ptr.To(int64(80))))
			Expect(devices[1].Name).To(Equal(unique.Make("gpu-1")))
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

		It("should return devices directly from the template", func() {
			devices := slice.Devices()
			Expect(devices).To(HaveLen(1))
			Expect(devices[0].Name).To(Equal(unique.Make("gpu-0")))
			Expect(devices[0].Attributes["gpu.nvidia.com/model"].StringValue).To(Equal(ptr.To("A100")))
		})

		It("should return nil NodeSelector", func() {
			Expect(slice.NodeSelector()).To(BeNil())
		})

		It("should return false for AllNodes", func() {
			Expect(slice.AllNodes()).To(BeFalse())
		})
	})
})
