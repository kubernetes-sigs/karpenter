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

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var (
	KwokZones = []string{"test-zone-a", "test-zone-b", "test-zone-c", "test-zone-d"}
)

func makeGenericInstanceTypeName(cpu, memFactor int, arch string, os v1.OSName) string {
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
	return fmt.Sprintf("%s-%s-%s-%s", family, size, arch, os)
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

func constructGenericInstanceTypes() []kwok.InstanceTypeOptions {
	var instanceTypesOptions []kwok.InstanceTypeOptions

	for _, cpu := range []int{1, 2, 4, 8, 16, 32, 48, 64, 96, 128, 192, 256} {
		for _, memFactor := range []int{2, 4, 8} {
			for _, os := range []v1.OSName{v1.Linux, v1.Windows} {
				for _, arch := range []string{v1beta1.ArchitectureAmd64, v1beta1.ArchitectureArm64} {
					// Construct instance type details, then construct offerings.
					name := makeGenericInstanceTypeName(cpu, memFactor, arch, os)
					mem := cpu * memFactor
					pods := lo.Clamp(cpu*16, 0, 1024)
					opts := kwok.InstanceTypeOptions{
						Name:             name,
						Architecture:     arch,
						OperatingSystems: []v1.OSName{os},
						Resources: v1.ResourceList{
							v1.ResourceCPU:              resource.MustParse(fmt.Sprintf("%d", cpu)),
							v1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dGi", mem)),
							v1.ResourcePods:             resource.MustParse(fmt.Sprintf("%d", pods)),
							v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
						},
					}
					price := priceFromResources(opts.Resources)

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
					instanceTypesOptions = append(instanceTypesOptions, opts)
				}
			}
		}
	}
	return instanceTypesOptions
}

func main() {
	opts := constructGenericInstanceTypes()
	output, err := json.MarshalIndent(opts, "", "    ")
	if err != nil {
		fmt.Printf("could not marshal generated instance types to JSON: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(string(output))
}
