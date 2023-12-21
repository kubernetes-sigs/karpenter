/*
Copyright 2023 The Kubernetes Authors.

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

package kwok

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type InstanceTypeOptions struct {
	Name               string
	Offerings          cloudprovider.Offerings
	Architecture       string
	OperatingSystems   sets.Set[string]
	Resources          v1.ResourceList
	InstanceTypeLabels map[string]string
}

func MakeInstanceTypeLabels(cpu, memFactor int) map[string]string {
	size := fmt.Sprintf("%dx", cpu)
	integer := fmt.Sprintf("%d", cpu)
	var family string
	switch memFactor {
	case 2:
		family = "c" // cpu
	case 4:
		family = "s" // standard
	case 8:
		family = "m" // memory
	default:
		family = "e" // exotic
	}
	return map[string]string{InstanceSizeLabelKey: size, InstanceFamilyLabelKey: family, IntegerInstanceLabelKey: integer}
}

// InstanceTypesAssorted create many unique instance types with varying CPU/memory/architecture/OS/zone/capacity type.
func ConstructInstanceTypes() []*cloudprovider.InstanceType {
	var instanceTypes []*cloudprovider.InstanceType
	for _, cpu := range []int{1, 2, 4, 8, 16, 32, 48, 64, 96, 128, 192, 256} {
		for _, memFactor := range []int{2, 4, 8} {
			for _, os := range []sets.Set[string]{sets.New(string(v1.Linux)), sets.New(string(v1.Windows))} {
				for _, arch := range []string{v1beta1.ArchitectureAmd64, v1beta1.ArchitectureArm64} {
					// Construct instance type details, then construct offerings.
					mem := cpu * memFactor
					pods := lo.Clamp(cpu*16, 0, 1024)
					labels := MakeInstanceTypeLabels(cpu, memFactor)
					opts := InstanceTypeOptions{
						Name:             fmt.Sprintf("%s-%s-%s-%s", labels[InstanceFamilyLabelKey], labels[InstanceSizeLabelKey], arch, strings.Join(sets.List(os), ",")),
						Architecture:     arch,
						OperatingSystems: os,
						Resources: v1.ResourceList{
							v1.ResourceCPU:              resource.MustParse(fmt.Sprintf("%d", cpu)),
							v1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dGi", mem)),
							v1.ResourcePods:             resource.MustParse(fmt.Sprintf("%d", pods)),
							v1.ResourceEphemeralStorage: resource.MustParse("20G"),
						},
						InstanceTypeLabels: labels,
					}
					price := priceFromResources(opts.Resources)

					opts.Offerings = cloudprovider.Offerings{}
					for _, zone := range KwokZones {
						for _, ct := range []string{v1beta1.CapacityTypeSpot, v1beta1.CapacityTypeOnDemand} {
							if ct == v1beta1.CapacityTypeSpot {
								// add no lint here as gosec fires a false positive here
								//nolint
								jitter := rand.Float64()*2 - .01
								price = price * (.7 + jitter)
							}
							opts.Offerings = append(opts.Offerings, cloudprovider.Offering{
								CapacityType: ct,
								Zone:         zone,
								Price:        price,
								Available:    true,
							})
						}
					}
					instanceTypes = append(instanceTypes, newInstanceType(opts))
				}
			}
		}
	}
	return instanceTypes
}

func newInstanceType(options InstanceTypeOptions) *cloudprovider.InstanceType {
	requirements := scheduling.NewRequirements(
		scheduling.NewRequirement(v1.LabelInstanceTypeStable, v1.NodeSelectorOpIn, options.Name),
		scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, options.Architecture),
		scheduling.NewRequirement(v1.LabelOSStable, v1.NodeSelectorOpIn, sets.List(options.OperatingSystems)...),
		scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, lo.Map(options.Offerings.Available(), func(o cloudprovider.Offering, _ int) string { return o.Zone })...),
		scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, lo.Map(options.Offerings.Available(), func(o cloudprovider.Offering, _ int) string { return o.CapacityType })...),
		scheduling.NewRequirement(InstanceSizeLabelKey, v1.NodeSelectorOpIn, options.InstanceTypeLabels[InstanceSizeLabelKey]),
		scheduling.NewRequirement(InstanceFamilyLabelKey, v1.NodeSelectorOpIn, options.InstanceTypeLabels[InstanceFamilyLabelKey]),
		scheduling.NewRequirement(IntegerInstanceLabelKey, v1.NodeSelectorOpIn, options.InstanceTypeLabels[IntegerInstanceLabelKey]),
	)

	return &cloudprovider.InstanceType{
		Name:         options.Name,
		Requirements: requirements,
		Offerings:    options.Offerings,
		Capacity:     options.Resources,
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
	}
}

func priceFromResources(resources v1.ResourceList) float64 {
	price := 0.0
	for k, v := range resources {
		switch k {
		case v1.ResourceCPU:
			price += 0.025 * v.AsApproximateFloat64()
		case v1.ResourceMemory:
			price += 0.001 * v.AsApproximateFloat64() / (1e9)
			// case ResourceGPUVendorA, ResourceGPUVendorB:
			// 	price += 1.0
		}
	}
	return price
}
