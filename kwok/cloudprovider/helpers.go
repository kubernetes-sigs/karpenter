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

package kwok

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

var (
	// AWS uses (family).(size) format
	awsRegexp = regexp.MustCompile(`.*\.(small|medium|large|\d*xlarge)`)
)

type InstanceTypeOptions struct {
	Name             string                  `json:"name"`
	Offerings        cloudprovider.Offerings `json:"offerings"`
	Architecture     string                  `json:"architecture"`
	OperatingSystems sets.Set[string]        `json:"operatingSystems"`
	Resources        v1.ResourceList         `json:"resources"`

	// These are used for setting default requirements, they should not be used
	// for setting arbitrary node labels.  Set the labels on the created NodePool for
	// that use case.
	instanceTypeLabels map[string]string
}

// ConstructInstanceTypes create many unique instance types with varying CPU/memory/architecture/OS/zone/capacity type.
func ConstructInstanceTypes() ([]*cloudprovider.InstanceType, error) {
	if instanceTypeFile := os.Getenv(kwokInstanceTypeFileKey); instanceTypeFile != "" {
		return constructConfiguredInstanceTypes(instanceTypeFile)
	} else {
		return constructStaticInstanceTypes(), nil
	}
}

func constructConfiguredInstanceTypes(instanceTypeFile string) ([]*cloudprovider.InstanceType, error) {
	var instanceTypes []*cloudprovider.InstanceType
	var instanceTypeOptions []InstanceTypeOptions

	data, err := os.ReadFile(instanceTypeFile)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", instanceTypeFile, err)
	}

	if err = json.Unmarshal(data, &instanceTypeOptions); err != nil {
		return nil, fmt.Errorf("could not parse JSON data: %w", err)
	}

	for _, opts := range instanceTypeOptions {
		opts = setDefaultOptions(opts)
		instanceTypes = append(instanceTypes, newInstanceType(opts))
	}
	return instanceTypes, nil
}

func parseSizeFromType(ty, cpu string) string {
	if matches := awsRegexp.FindStringSubmatch(ty); matches != nil {
		return matches[1]
	}

	// If we can't figure out the size, fall back to the cpu value;
	// this works for both Azure and GCP
	return cpu
}

func setDefaultOptions(opts InstanceTypeOptions) InstanceTypeOptions {
	var cpu, memory string
	for res, q := range opts.Resources {
		switch res {
		case "cpu":
			cpu = q.String()
		case "memory":
			memory = q.String()
		}
	}
	family := opts.Name[0:1] // all of the three major compute clouds (AWS, Azure, GCP) use the first letter of the instance name as the family
	opts.instanceTypeLabels = map[string]string{
		InstanceTypeLabelKey:   opts.Name,
		InstanceSizeLabelKey:   parseSizeFromType(opts.Name, cpu),
		InstanceFamilyLabelKey: family,
		InstanceCPULabelKey:    cpu,
		InstanceMemoryLabelKey: memory,
	}

	// if the user specified a different pod limit, override the default
	opts.Resources = lo.Assign(v1.ResourceList{
		v1.ResourcePods: resource.MustParse("110"), // Default number of pods on a node in Kubernetes
	}, opts.Resources)

	// make sure all the instance types are available
	for i := range opts.Offerings {
		opts.Offerings[i].Available = true
	}

	return opts
}

func constructStaticInstanceTypes() []*cloudprovider.InstanceType {
	var instanceTypes []*cloudprovider.InstanceType
	for _, cpu := range []int{1, 2, 4, 8, 16, 32, 48, 64, 96, 128, 192, 256} {
		for _, memFactor := range []int{2, 4, 8} {
			for _, os := range []sets.Set[string]{sets.New(string(v1.Linux)), sets.New(string(v1.Windows))} {
				for _, arch := range []string{v1beta1.ArchitectureAmd64, v1beta1.ArchitectureArm64} {
					// Construct instance type details, then construct offerings.
					mem := cpu * memFactor
					pods := lo.Clamp(cpu*16, 0, 1024)
					labels := makeStaticInstanceTypeLabels(cpu, memFactor, arch, os)
					opts := InstanceTypeOptions{
						Name:             labels[InstanceTypeLabelKey],
						Architecture:     arch,
						OperatingSystems: os,
						Resources: v1.ResourceList{
							v1.ResourceCPU:              resource.MustParse(fmt.Sprintf("%d", cpu)),
							v1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dGi", mem)),
							v1.ResourcePods:             resource.MustParse(fmt.Sprintf("%d", pods)),
							v1.ResourceEphemeralStorage: resource.MustParse("20G"),
						},
						instanceTypeLabels: labels,
					}
					price := PriceFromResources(opts.Resources)

					opts.Offerings = cloudprovider.Offerings{}
					for _, zone := range KwokZones {
						for _, ct := range []string{v1beta1.CapacityTypeSpot, v1beta1.CapacityTypeOnDemand} {
							opts.Offerings = append(opts.Offerings, cloudprovider.Offering{
								CapacityType: ct,
								Zone:         zone,
								Price:        lo.Ternary(ct == v1beta1.CapacityTypeSpot, price*.7, price),
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

func makeStaticInstanceTypeLabels(cpu, memFactor int, arch string, os sets.Set[string]) map[string]string {
	size := fmt.Sprintf("%dx", cpu)
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
	ty := fmt.Sprintf("%s-%s-%s-%s", family, size, arch, strings.Join(sets.List(os), ","))
	return map[string]string{
		InstanceTypeLabelKey:   ty,
		InstanceSizeLabelKey:   size,
		InstanceFamilyLabelKey: family,
		InstanceCPULabelKey:    fmt.Sprintf("%d", cpu),
		InstanceMemoryLabelKey: fmt.Sprintf("%d", cpu*memFactor*1024),
	}
}

func newInstanceType(options InstanceTypeOptions) *cloudprovider.InstanceType {
	requirements := scheduling.NewRequirements(
		scheduling.NewRequirement(v1.LabelInstanceTypeStable, v1.NodeSelectorOpIn, options.Name),
		scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, options.Architecture),
		scheduling.NewRequirement(v1.LabelOSStable, v1.NodeSelectorOpIn, sets.List(options.OperatingSystems)...),
		scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, lo.Map(options.Offerings.Available(), func(o cloudprovider.Offering, _ int) string { return o.Zone })...),
		scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, lo.Map(options.Offerings.Available(), func(o cloudprovider.Offering, _ int) string { return o.CapacityType })...),
		scheduling.NewRequirement(InstanceSizeLabelKey, v1.NodeSelectorOpIn, options.instanceTypeLabels[InstanceSizeLabelKey]),
		scheduling.NewRequirement(InstanceFamilyLabelKey, v1.NodeSelectorOpIn, options.instanceTypeLabels[InstanceFamilyLabelKey]),
		scheduling.NewRequirement(InstanceCPULabelKey, v1.NodeSelectorOpIn, options.instanceTypeLabels[InstanceCPULabelKey]),
		scheduling.NewRequirement(InstanceMemoryLabelKey, v1.NodeSelectorOpIn, options.instanceTypeLabels[InstanceMemoryLabelKey]),
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

func PriceFromResources(resources v1.ResourceList) float64 {
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
