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

			// Mutating the copy should not affect the original
			copied.ResourceSliceTemplates[0].Devices[0].Attributes["gpu.nvidia.com/model"] = resourcev1.DeviceAttribute{
				StringValue: ptr.To("A100"),
			}
			Expect(*original.ResourceSliceTemplates[0].Devices[0].Attributes["gpu.nvidia.com/model"].StringValue).To(Equal("H100"))

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
	})
})
