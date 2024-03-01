package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var (
	KwokZones = []string{"test-zone-a", "test-zone-b", "test-zone-c", "test-zone-d"}
)

func makeGenericInstanceTypeName(cpu, memFactor int, arch string, os sets.Set[string]) string {
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
	return fmt.Sprintf("%s-%s-%s-%s", family, size, arch, strings.Join(sets.List(os), ","))
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
			for _, os := range []sets.Set[string]{sets.New(string(v1.Linux)), sets.New(string(v1.Windows))} {
				for _, arch := range []string{v1beta1.ArchitectureAmd64, v1beta1.ArchitectureArm64} {
					// Construct instance type details, then construct offerings.
					name := makeGenericInstanceTypeName(cpu, memFactor, arch, os)
					mem := cpu * memFactor
					pods := lo.Clamp(cpu*16, 0, 1024)
					opts := kwok.InstanceTypeOptions{
						Name:             name,
						Architecture:     arch,
						OperatingSystems: os,
						Resources: v1.ResourceList{
							v1.ResourceCPU:              resource.MustParse(fmt.Sprintf("%d", cpu)),
							v1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dGi", mem)),
							v1.ResourcePods:             resource.MustParse(fmt.Sprintf("%d", pods)),
							v1.ResourceEphemeralStorage: resource.MustParse("20G"),
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
