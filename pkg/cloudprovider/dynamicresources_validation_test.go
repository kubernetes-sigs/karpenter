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
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	gpuModeKey = "example.com/driver-mode-gpu"
	nicModeKey = "example.com/driver-mode-nic"
	modeDP     = "device-plugin"
	modeDRA    = "dynamic-resource-allocation"
)

// modeInstanceType builds an instance type with one offering per combination of GPU and NIC driver modes, a GPU set
// published in GPU DRA mode, and a NIC set published in NIC DRA mode.
func modeInstanceType() *cloudprovider.InstanceType {
	var offerings cloudprovider.Offerings
	for _, gpu := range []string{modeDP, modeDRA} {
		for _, nic := range []string{modeDP, modeDRA} {
			offerings = append(offerings, &cloudprovider.Offering{
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"),
					scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
					scheduling.NewRequirement(gpuModeKey, corev1.NodeSelectorOpIn, gpu),
					scheduling.NewRequirement(nicModeKey, corev1.NodeSelectorOpIn, nic),
				),
				Price:     1.0,
				Available: true,
			})
		}
	}
	return &cloudprovider.InstanceType{
		Name: "it",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(gpuModeKey, corev1.NodeSelectorOpIn, modeDP, modeDRA),
			scheduling.NewRequirement(nicModeKey, corev1.NodeSelectorOpIn, modeDP, modeDRA),
		),
		Offerings: offerings,
		DynamicResources: []*cloudprovider.DynamicResources{
			modeSet(gpuModeKey, modeDRA, template("gpu.example.com", "gpus", "gpu-0")),
			modeSet(nicModeKey, modeDRA, template("nic.example.com", "nics", "nic-0")),
		},
	}
}

func modeSet(key, value string, templates ...*cloudprovider.ResourceSliceTemplate) *cloudprovider.DynamicResources {
	return &cloudprovider.DynamicResources{
		Requirements:           scheduling.NewRequirements(scheduling.NewRequirement(key, corev1.NodeSelectorOpIn, value)),
		ResourceSliceTemplates: templates,
	}
}

func template(driver, pool string, devices ...string) *cloudprovider.ResourceSliceTemplate {
	t := &cloudprovider.ResourceSliceTemplate{Driver: unique.Make(driver), Pool: cloudprovider.ResourcePool{Name: unique.Make(pool)}}
	for _, d := range devices {
		t.Devices = append(t.Devices, cloudprovider.Device{Name: unique.Make(d)})
	}
	return t
}

var _ = Describe("DynamicResources sets", func() {
	BeforeEach(func() {
		v1.WellKnownLabels.Insert(gpuModeKey, nicModeKey)
		DeferCleanup(func() { v1.WellKnownLabels.Delete(gpuModeKey, nicModeKey) })
	})

	Describe("AvailableDynamicResources", func() {
		It("should include sets with an available compatible offering", func() {
			it := modeInstanceType()
			Expect(it.AvailableDynamicResources()).To(HaveLen(2))
		})
		It("should exclude a scoped set whose compatible offerings are all unavailable", func() {
			it := modeInstanceType()
			for _, o := range it.Offerings {
				if o.Requirements.Get(gpuModeKey).Has(modeDRA) {
					o.Available = false
				}
			}
			Expect(it.AvailableDynamicResources()).To(ConsistOf(it.DynamicResources[1]))
		})
		It("should always include an unscoped set", func() {
			it := modeInstanceType()
			it.DynamicResources = append(it.DynamicResources, &cloudprovider.DynamicResources{
				ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{template("tpu.example.com", "tpus", "tpu-0")},
			})
			for _, o := range it.Offerings {
				o.Available = false
			}
			Expect(it.AvailableDynamicResources()).To(ConsistOf(it.DynamicResources[2]))
		})
		It("should compute the result once per instance type", func() {
			it := modeInstanceType()
			Expect(it.AvailableDynamicResources()).To(HaveLen(2))
			for _, o := range it.Offerings {
				o.Available = false
			}
			Expect(it.AvailableDynamicResources()).To(HaveLen(2))
			Expect(it.DeepCopy().AvailableDynamicResources()).To(BeEmpty())
		})
	})

	Describe("ValidateDynamicResources", func() {
		It("should accept an instance type that follows the conventions", func() {
			Expect(cloudprovider.ValidateDynamicResources(modeInstanceType())).To(BeEmpty())
		})
		It("should accept an instance type without DynamicResources", func() {
			Expect(cloudprovider.ValidateDynamicResources(&cloudprovider.InstanceType{Name: "it"})).To(BeEmpty())
		})
		It("should reject a set with templates for multiple drivers", func() {
			it := modeInstanceType()
			it.DynamicResources[0].ResourceSliceTemplates = append(it.DynamicResources[0].ResourceSliceTemplates, template("nic.example.com", "other-nics", "nic-1"))
			Expect(cloudprovider.ValidateDynamicResources(it)).To(ContainElement(MatchError(ContainSubstring("multiple drivers"))))
		})
		It("should reject an offering that's compatible with two sets of the same driver", func() {
			it := modeInstanceType()
			it.DynamicResources = append(it.DynamicResources, modeSet(nicModeKey, modeDRA, template("gpu.example.com", "more-gpus", "gpu-1")))
			Expect(cloudprovider.ValidateDynamicResources(it)).To(ContainElement(MatchError(ContainSubstring(`of driver "gpu.example.com"`))))
		})
		It("should reject a set that isn't compatible with any offering", func() {
			it := modeInstanceType()
			for _, o := range it.Offerings {
				o.Requirements[gpuModeKey] = scheduling.NewRequirement(gpuModeKey, corev1.NodeSelectorOpIn, modeDP)
			}
			Expect(cloudprovider.ValidateDynamicResources(it)).To(ContainElement(MatchError(ContainSubstring("not compatible with any offering"))))
		})
		It("should reject a device that appears in more than one set", func() {
			it := modeInstanceType()
			it.DynamicResources = append(it.DynamicResources, modeSet(gpuModeKey, modeDP, template("gpu.example.com", "gpus", "gpu-0")))
			Expect(cloudprovider.ValidateDynamicResources(it)).To(ContainElement(MatchError(ContainSubstring("gpu.example.com/gpus/gpu-0 is in sets"))))
		})
		It("should reject a set key that isn't a well-known label", func() {
			v1.WellKnownLabels.Delete(nicModeKey)
			Expect(cloudprovider.ValidateDynamicResources(modeInstanceType())).To(ContainElement(MatchError(ContainSubstring("not a well-known label"))))
		})
		It("should reject instance type requirements that don't define a set key", func() {
			it := modeInstanceType()
			delete(it.Requirements, gpuModeKey)
			Expect(cloudprovider.ValidateDynamicResources(it)).To(ContainElement(MatchError(ContainSubstring("don't define set key"))))
		})
		It("should reject instance type requirements that aren't the union of the offerings' values", func() {
			it := modeInstanceType()
			it.Requirements[gpuModeKey] = scheduling.NewRequirement(gpuModeKey, corev1.NodeSelectorOpIn, modeDRA)
			Expect(cloudprovider.ValidateDynamicResources(it)).To(ContainElement(MatchError(ContainSubstring("but its offerings carry"))))
		})
		It("should reject a DoesNotExist set key that offerings carry values for", func() {
			it := modeInstanceType()
			it.Requirements[gpuModeKey] = scheduling.NewRequirement(gpuModeKey, corev1.NodeSelectorOpDoesNotExist)
			Expect(cloudprovider.ValidateDynamicResources(it)).To(ContainElement(MatchError(ContainSubstring("is DoesNotExist, but offerings carry"))))
		})
		It("should reject offerings whose price varies by mode", func() {
			it := modeInstanceType()
			it.Offerings[3].Price = 2.0
			Expect(cloudprovider.ValidateDynamicResources(it)).To(ContainElement(MatchError(ContainSubstring("differ only in set keys"))))
		})
	})
})
