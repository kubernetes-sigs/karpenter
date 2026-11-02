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
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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

var _ = Describe("DynamicResources", func() {
	Describe("DeepCopy", func() {
		It("should deep copy Requirements and ResourceSliceTemplates", func() {
			original := cloudprovider.DynamicResources{
				Requirements: scheduling.NewRequirements(scheduling.NewRequirement("example.com/driver-mode", corev1.NodeSelectorOpIn, "dra")),
				ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{
					{
						Driver: unique.Make("gpu.nvidia.com"),
						Pool:   cloudprovider.ResourcePool{Name: unique.Make("gpus")},
						Devices: []cloudprovider.Device{
							{
								Name: unique.Make("gpu-0"),
								Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
									"gpu.nvidia.com/model": {StringValue: new("H100")},
								},
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.nvidia.com/memory": {Value: resource.MustParse("80Gi")},
								},
								AllowMultipleAllocations: true,
							},
						},
					},
				},
			}

			copied := original.DeepCopy()

			Expect(copied.Requirements.Get("example.com/driver-mode").Has("dra")).To(BeTrue())
			Expect(copied.ResourceSliceTemplates).To(HaveLen(1))
			Expect(copied.ResourceSliceTemplates[0].Driver).To(Equal(unique.Make("gpu.nvidia.com")))
			Expect(copied.ResourceSliceTemplates[0].Devices).To(HaveLen(1))

			dev := copied.ResourceSliceTemplates[0].Devices[0]
			Expect(dev.AllowMultipleAllocations).To(BeTrue())
			Expect(dev.Capacity).To(HaveLen(1))
			Expect(dev.Capacity["gpu.nvidia.com/memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())

			// Mutating the copy should not affect the original
			copied.Requirements.Get("example.com/driver-mode").Insert("device-plugin")
			Expect(original.Requirements.Get("example.com/driver-mode").Has("device-plugin")).To(BeFalse())
			copied.ResourceSliceTemplates[0].Devices[0].Attributes["gpu.nvidia.com/model"] = resourcev1.DeviceAttribute{
				StringValue: new("A100"),
			}
			Expect(*original.ResourceSliceTemplates[0].Devices[0].Attributes["gpu.nvidia.com/model"].StringValue).To(Equal("H100"))

			dev.Capacity["gpu.nvidia.com/memory"] = resourcev1.DeviceCapacity{Value: resource.MustParse("40Gi")}
			Expect(original.ResourceSliceTemplates[0].Devices[0].Capacity["gpu.nvidia.com/memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())
		})

		It("should deep copy an instance type's DynamicResources sets and AttributeBindings", func() {
			original := &cloudprovider.InstanceType{
				Name: "it",
				DynamicResources: []*cloudprovider.DynamicResources{
					{ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("gpu.nvidia.com")}}},
					{
						Requirements:           scheduling.NewRequirements(scheduling.NewRequirement("example.com/driver-mode", corev1.NodeSelectorOpIn, "dra")),
						ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("dra.net")}},
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

			Expect(copied.DynamicResources).To(HaveLen(2))
			Expect(copied.DynamicResources[1]).ToNot(BeIdenticalTo(original.DynamicResources[1]))
			Expect(copied.DynamicResources[1].ResourceSliceTemplates[0].Driver).To(Equal(unique.Make("dra.net")))
			Expect(copied.AttributeBindings).To(HaveLen(1))
			Expect(copied.AttributeBindings[0].Devices).To(HaveLen(2))

			copied.DynamicResources[1].Requirements.Get("example.com/driver-mode").Insert("device-plugin")
			Expect(original.DynamicResources[1].Requirements.Get("example.com/driver-mode").Has("device-plugin")).To(BeFalse())
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
			Expect(copied.Requirements).To(BeNil())
			Expect(copied.ResourceSliceTemplates).To(BeNil())
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

	Describe("AvailableDynamicResources", func() {
		BeforeEach(func() {
			// Set keys must be well-known (see the DynamicResources conventions).
			v1.WellKnownLabels.Insert(gpuModeKey, nicModeKey)
			DeferCleanup(func() { v1.WellKnownLabels.Delete(gpuModeKey, nicModeKey) })
		})
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
			Expect(it.AvailableDynamicResources()).To(ConsistOf(it.AllDynamicResources()[1]))
		})
		It("should always include an unscoped set", func() {
			it := modeInstanceType()
			it.DynamicResources = append(it.DynamicResources, &cloudprovider.DynamicResources{
				ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{template("tpu.example.com", "tpus", "tpu-0")},
			})
			for _, o := range it.Offerings {
				o.Available = false
			}
			Expect(it.AvailableDynamicResources()).To(ConsistOf(it.AllDynamicResources()[2]))
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

	Describe("Namespacing", func() {
		// reusedInstanceType has two GPU sets that reuse the same pool and device names, and a NIC set.
		reusedInstanceType := func() *cloudprovider.InstanceType {
			it := modeInstanceType()
			it.DynamicResources = []*cloudprovider.DynamicResources{
				{Requirements: scheduling.NewRequirements(scheduling.NewRequirement(gpuModeKey, corev1.NodeSelectorOpIn, modeDRA)),
					ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{template("gpu.example.com", "gpus", "gpu-0", "gpu-1")}},
				{Requirements: scheduling.NewRequirements(scheduling.NewRequirement(gpuModeKey, corev1.NodeSelectorOpIn, modeDP)),
					ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{template("gpu.example.com", "gpus", "gpu-0", "gpu-1")}},
				{Requirements: scheduling.NewRequirements(scheduling.NewRequirement(nicModeKey, corev1.NodeSelectorOpIn, modeDRA)),
					ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{template("nic.example.com", "nics", "nic-0")}},
			}
			return it
		}
		poolsOf := func(sets []*cloudprovider.DynamicResources) []string {
			return lo.FlatMap(sets, func(dr *cloudprovider.DynamicResources, _ int) []string {
				return lo.Map(dr.ResourceSliceTemplates, func(t *cloudprovider.ResourceSliceTemplate, _ int) string { return t.Pool.Name.Value() })
			})
		}
		id := func(driver, pool, device string) cloudprovider.DeviceID {
			return cloudprovider.DeviceID{Driver: unique.Make(driver), Pool: unique.Make(pool), Device: unique.Make(device)}
		}

		BeforeEach(func() {
			v1.WellKnownLabels.Insert(gpuModeKey, nicModeKey)
			DeferCleanup(func() { v1.WellKnownLabels.Delete(gpuModeKey, nicModeKey) })
		})

		It("should namespace each set's template pools by the set's index", func() {
			it := reusedInstanceType()
			Expect(poolsOf(it.AllDynamicResources())).To(Equal([]string{
				cloudprovider.TemplatePoolName("gpus", 0), cloudprovider.TemplatePoolName("gpus", 1), cloudprovider.TemplatePoolName("nics", 2),
			}))
		})
		It("should keep sets that reuse pool and device names distinct", func() {
			sets := reusedInstanceType().AllDynamicResources()
			Expect(sets[0].ResourceSliceTemplates[0].Pool).ToNot(Equal(sets[1].ResourceSliceTemplates[0].Pool))
			Expect(sets[0].ResourceSliceTemplates[0].Devices).To(Equal(sets[1].ResourceSliceTemplates[0].Devices))
		})
		It("should namespace an unscoped single set", func() {
			it := &cloudprovider.InstanceType{DynamicResources: []*cloudprovider.DynamicResources{
				{ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{template("gpu.example.com", "gpus", "gpu-0")}},
			}}
			Expect(poolsOf(it.AllDynamicResources())).To(Equal([]string{cloudprovider.TemplatePoolName("gpus", 0)}))
			Expect(it.AllDynamicResources()[0].Requirements).To(BeEmpty())
		})
		It("should namespace a template whose pool is unset", func() {
			it := &cloudprovider.InstanceType{DynamicResources: []*cloudprovider.DynamicResources{
				{ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("gpu.example.com")}}},
			}}
			Expect(poolsOf(it.AllDynamicResources())).To(Equal([]string{cloudprovider.TemplatePoolName("", 0)}))
		})
		It("should not modify the provider's templates", func() {
			it := reusedInstanceType()
			original := it.DynamicResources[0].ResourceSliceTemplates[0]
			namespaced := it.AllDynamicResources()[0].ResourceSliceTemplates[0]
			Expect(namespaced).ToNot(BeIdenticalTo(original))
			Expect(original.Pool.Name.Value()).To(Equal("gpus"))
			Expect(poolsOf(it.DynamicResources)).To(Equal([]string{"gpus", "gpus", "nics"}))
		})
		It("should compute the namespaced sets once, and recompute them for a deep copy", func() {
			it := reusedInstanceType()
			first := it.AllDynamicResources()
			Expect(it.AllDynamicResources()).To(Equal(first))
			Expect(it.AllDynamicResources()[0]).To(BeIdenticalTo(first[0]))
			copied := it.DeepCopy().AllDynamicResources()
			Expect(copied[0]).ToNot(BeIdenticalTo(first[0]))
			Expect(poolsOf(copied)).To(Equal(poolsOf(first)))
		})
		It("should return the available subset of the namespaced sets", func() {
			it := reusedInstanceType()
			for _, o := range it.Offerings {
				if o.Requirements.Get(gpuModeKey).Has(modeDP) {
					o.Available = false
				}
			}
			all := it.AllDynamicResources()
			Expect(all).To(HaveLen(3))
			Expect(it.AvailableDynamicResources()).To(ConsistOf(all[0], all[2]))
		})
		It("should expand a binding to every set's copy of a bound device", func() {
			it := reusedInstanceType()
			it.AttributeBindings = []*cloudprovider.AttributeBinding{{
				Attribute: "resource.kubernetes.io/pcieRoot",
				Devices:   []cloudprovider.DeviceID{id("gpu.example.com", "gpus", "gpu-0"), id("nic.example.com", "nics", "nic-0")},
			}}
			bindings := it.ResolvedAttributeBindings()
			Expect(bindings).To(HaveLen(1))
			Expect(bindings[0].Attribute).To(Equal(resourcev1.QualifiedName("resource.kubernetes.io/pcieRoot")))
			Expect(bindings[0].Devices).To(ConsistOf(
				id("gpu.example.com", cloudprovider.TemplatePoolName("gpus", 0), "gpu-0"),
				id("gpu.example.com", cloudprovider.TemplatePoolName("gpus", 1), "gpu-0"),
				id("nic.example.com", cloudprovider.TemplatePoolName("nics", 2), "nic-0"),
			))
			Expect(it.AttributeBindings[0].Devices).To(ConsistOf(id("gpu.example.com", "gpus", "gpu-0"), id("nic.example.com", "nics", "nic-0")))
		})
		It("should keep a bound device that appears in no set unchanged", func() {
			it := reusedInstanceType()
			unknown := id("gpu.example.com", "elsewhere", "gpu-9")
			it.AttributeBindings = []*cloudprovider.AttributeBinding{{Attribute: "example.com/numa", Devices: []cloudprovider.DeviceID{unknown}}}
			Expect(it.ResolvedAttributeBindings()[0].Devices).To(ConsistOf(unknown))
		})
		It("should round-trip namespaced pool names", func() {
			pool, setIndex, ok := cloudprovider.ParseTemplatePoolName(cloudprovider.TemplatePoolName("gpus-node.example/a", 12))
			Expect(ok).To(BeTrue())
			Expect(pool).To(Equal("gpus-node.example/a"))
			Expect(setIndex).To(Equal(12))
			_, _, ok = cloudprovider.ParseTemplatePoolName("gpus")
			Expect(ok).To(BeFalse())
			_, _, ok = cloudprovider.ParseTemplatePoolName("gpus~x")
			Expect(ok).To(BeFalse())
		})
	})
})
