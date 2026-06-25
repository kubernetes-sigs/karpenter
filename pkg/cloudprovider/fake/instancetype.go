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

package fake

import (
	"fmt"
	"strings"
	"unique"

	"github.com/awslabs/operatorpkg/option"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	LabelInstanceSize                           = "size"
	ExoticInstanceLabelKey                      = "special"
	IntegerInstanceLabelKey                     = "integer"
	ResourceGPUVendorA      corev1.ResourceName = "fake.com/vendor-a"
	ResourceGPUVendorB      corev1.ResourceName = "fake.com/vendor-b"
)

func init() {
	v1.WellKnownLabels.Insert(
		LabelInstanceSize,
		ExoticInstanceLabelKey,
		IntegerInstanceLabelKey,
	)
}

type InstanceTypeOptions = option.Function[InstanceTypeConfig]

type InstanceTypeConfig struct {
	Resources              corev1.ResourceList
	Offerings              cloudprovider.Offerings
	Architecture           string
	OperatingSystems       sets.Set[string]
	ResourceSliceTemplates []*cloudprovider.ResourceSliceTemplate
	AttributeBindings      []*cloudprovider.AttributeBinding
	Requirements           []*scheduling.Requirement
}

func WithResources(resources corev1.ResourceList) InstanceTypeOptions {
	return func(c *InstanceTypeConfig) { c.Resources = resources }
}

func WithOfferings(offerings ...cloudprovider.Offering) InstanceTypeOptions {
	return func(c *InstanceTypeConfig) {
		c.Offerings = lo.ToSlicePtr(offerings)
	}
}

func WithArchitecture(arch string) InstanceTypeOptions {
	return func(c *InstanceTypeConfig) { c.Architecture = arch }
}

func WithOperatingSystems(os ...string) InstanceTypeOptions {
	return func(c *InstanceTypeConfig) { c.OperatingSystems = sets.New(os...) }
}

func WithResourceSliceTemplates(templates ...cloudprovider.ResourceSliceTemplate) InstanceTypeOptions {
	return func(c *InstanceTypeConfig) {
		c.ResourceSliceTemplates = lo.ToSlicePtr(templates)
	}
}

func WithAttributeBindings(bindings ...cloudprovider.AttributeBinding) InstanceTypeOptions {
	return func(c *InstanceTypeConfig) {
		c.AttributeBindings = lo.ToSlicePtr(bindings)
	}
}

func WithRequirements(reqs ...*scheduling.Requirement) InstanceTypeOptions {
	return func(c *InstanceTypeConfig) { c.Requirements = reqs }
}

func NewInstanceType(name string, opts ...InstanceTypeOptions) *cloudprovider.InstanceType {
	cfg := option.Resolve(opts...)
	if cfg.Resources == nil {
		cfg.Resources = corev1.ResourceList{}
	}
	if r := cfg.Resources[corev1.ResourceCPU]; r.IsZero() {
		cfg.Resources[corev1.ResourceCPU] = resource.MustParse("4")
	}
	if r := cfg.Resources[corev1.ResourceMemory]; r.IsZero() {
		cfg.Resources[corev1.ResourceMemory] = resource.MustParse("4Gi")
	}
	if r := cfg.Resources[corev1.ResourcePods]; r.IsZero() {
		cfg.Resources[corev1.ResourcePods] = resource.MustParse("5")
	}
	if len(cfg.Offerings) == 0 {
		cfg.Offerings = cloudprovider.Offerings{
			{
				Available: true,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  "spot",
					corev1.LabelTopologyZone: "test-zone-1",
				}),
				Price: PriceFromResources(cfg.Resources),
			},
			{
				Available: true,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  "spot",
					corev1.LabelTopologyZone: "test-zone-2",
				}),
				Price: PriceFromResources(cfg.Resources),
			},
			{
				Available: true,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  "on-demand",
					corev1.LabelTopologyZone: "test-zone-1",
				}),
				Price: PriceFromResources(cfg.Resources),
			},
			{
				Available: true,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  "on-demand",
					corev1.LabelTopologyZone: "test-zone-2",
				}),
				Price: PriceFromResources(cfg.Resources),
			},
			{
				Available: true,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  "on-demand",
					corev1.LabelTopologyZone: "test-zone-3",
				}),
				Price: PriceFromResources(cfg.Resources),
			},
		}
	}
	if len(cfg.Architecture) == 0 {
		cfg.Architecture = "amd64"
	}
	if cfg.OperatingSystems.Len() == 0 {
		cfg.OperatingSystems = sets.New(string(corev1.Linux), string(corev1.Windows), "darwin")
	}
	requirements := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, cfg.Architecture),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, sets.List(cfg.OperatingSystems)...),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, lo.Map(cfg.Offerings.Available(), func(o *cloudprovider.Offering, _ int) string {
			return o.Requirements.Get(corev1.LabelTopologyZone).Any()
		})...),
		scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, lo.Map(cfg.Offerings.Available(), func(o *cloudprovider.Offering, _ int) string {
			return o.Requirements.Get(v1.CapacityTypeLabelKey).Any()
		})...),
		scheduling.NewRequirement(LabelInstanceSize, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(ExoticInstanceLabelKey, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(IntegerInstanceLabelKey, corev1.NodeSelectorOpIn, fmt.Sprint(cfg.Resources.Cpu().Value())),
	)
	for _, req := range cfg.Requirements {
		requirements.Add(req)
	}
	if cfg.Resources.Cpu().Cmp(resource.MustParse("4")) > 0 &&
		cfg.Resources.Memory().Cmp(resource.MustParse("8Gi")) > 0 {
		requirements.Get(LabelInstanceSize).Insert("large")
		requirements.Get(ExoticInstanceLabelKey).Insert("optional")
	} else {
		requirements.Get(LabelInstanceSize).Insert("small")
	}

	return &cloudprovider.InstanceType{
		Name:         name,
		Requirements: requirements,
		Offerings:    cfg.Offerings,
		Capacity:     cfg.Resources,
		DynamicResources: cloudprovider.DynamicResources{
			ResourceSliceTemplates: cfg.ResourceSliceTemplates,
			AttributeBindings:      cfg.AttributeBindings,
		},
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
	}
}

// ResourceSliceTemplate builds a cloud provider ResourceSliceTemplate with the given driver, pool, and devices.
// It is a convenience for tests constructing instance types with DRA device shapes via WithResourceSliceTemplates.
func ResourceSliceTemplate(driver, pool string, devices ...cloudprovider.Device) cloudprovider.ResourceSliceTemplate {
	return cloudprovider.ResourceSliceTemplate{
		Driver:  unique.Make(driver),
		Pool:    cloudprovider.ResourcePool{Name: unique.Make(pool)},
		Devices: devices,
	}
}

// Device builds a cloud provider DRA device with the given name and optional attributes.
func Device(name string, attributes map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) cloudprovider.Device {
	return cloudprovider.Device{
		Name:       unique.Make(name),
		Attributes: attributes,
	}
}

// Devices builds a slice of attribute-less DRA devices from the given names.
func Devices(names ...string) []cloudprovider.Device {
	return lo.Map(names, func(name string, _ int) cloudprovider.Device {
		return Device(name, nil)
	})
}

// InstanceTypesAssorted create many unique instance types with varying CPU/memory/architecture/OS/zone/capacity type.
func InstanceTypesAssorted() []*cloudprovider.InstanceType {
	var instanceTypes []*cloudprovider.InstanceType
	for _, cpu := range []int{1, 2, 4, 8, 16, 32, 64} {
		for _, mem := range []int{1, 2, 4, 8, 16, 32, 64, 128} {
			for _, zone := range []string{"test-zone-1", "test-zone-2", "test-zone-3"} {
				for _, ct := range []string{v1.CapacityTypeSpot, v1.CapacityTypeOnDemand} {
					for _, os := range []sets.Set[string]{sets.New(string(corev1.Linux)), sets.New(string(corev1.Windows))} {
						for _, arch := range []string{v1.ArchitectureAmd64, v1.ArchitectureArm64} {
							resources := corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", cpu)),
								corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", mem)),
							}
							price := PriceFromResources(resources)
							instanceTypes = append(instanceTypes, NewInstanceType(
								fmt.Sprintf("%d-cpu-%d-mem-%s-%s-%s-%s", cpu, mem, arch, strings.Join(sets.List(os), ","), zone, ct),
								WithArchitecture(arch),
								WithOperatingSystems(sets.List(os)...),
								WithResources(resources),
								WithOfferings(cloudprovider.Offering{
									Available: true,
									Requirements: scheduling.NewLabelRequirements(map[string]string{
										v1.CapacityTypeLabelKey:  ct,
										corev1.LabelTopologyZone: zone,
									}),
									Price: price,
								}),
							))
						}
					}
				}
			}
		}
	}
	return instanceTypes
}

// InstanceTypes creates instance types with incrementing resources
// 2Gi of RAM and 10 pods for every 1vcpu
// i.e. 1vcpu, 2Gi mem, 10 pods
//
//	2vcpu, 4Gi mem, 20 pods
//	3vcpu, 6Gi mem, 30 pods
func InstanceTypes(total int) []*cloudprovider.InstanceType {
	instanceTypes := []*cloudprovider.InstanceType{}
	for i := range total {
		instanceTypes = append(instanceTypes, NewInstanceType(
			fmt.Sprintf("fake-it-%d", i),
			WithResources(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", i+1)),
				corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", (i+1)*2)),
				corev1.ResourcePods:   resource.MustParse(fmt.Sprintf("%d", (i+1)*10)),
			}),
		))
	}
	return instanceTypes
}

func PriceFromResources(resources corev1.ResourceList) float64 {
	price := 0.0
	for k, v := range resources {
		switch k {
		case corev1.ResourceCPU:
			price += 0.1 * v.AsApproximateFloat64()
		case corev1.ResourceMemory:
			price += 0.1 * v.AsApproximateFloat64() / (1e9)
		case ResourceGPUVendorA, ResourceGPUVendorB:
			price += 1.0
		}
	}
	return price
}
