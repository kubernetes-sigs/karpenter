/*
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
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

var (
	instanceTypeScheme = regexp.MustCompile(`(^[a-z]+)(\-[0-9]+tb)?([0-9]+).*\.`)
)

func requirements(offerings cloudprovider.Offerings, it *cloudprovider.InstanceType) scheduling.Requirements {
	return scheduling.NewRequirements(
		scheduling.NewRequirement(v1.LabelInstanceTypeStable, v1.NodeSelectorOpIn, it.Name),
		scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, options.Architecture),
		scheduling.NewRequirement(v1.LabelOSStable, v1.NodeSelectorOpIn, sets.List(options.OperatingSystems)...),
		scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, lo.Map(options.Offerings.Available(), func(o cloudprovider.Offering, _ int) string { return o.Zone })...),
		scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, lo.Map(options.Offerings.Available(), func(o cloudprovider.Offering, _ int) string { return o.CapacityType })...),
		scheduling.NewRequirement(fake.LabelInstanceSize, v1.NodeSelectorOpIn, options.Size),
		scheduling.NewRequirement(fake.IntegerInstanceLabelKey, v1.NodeSelectorOpIn, fmt.Sprint(options.Resources.Cpu().Value())),
		// Add in
		scheduling.NewRequirement(fake.ExoticInstanceLabelKey, v1.NodeSelectorOpDoesNotExist),
	)
}

func uniqueCapacityType(available cloudprovider.Offerings) []string {
	uniq := map[string]struct{}{}
	for _, c := range available {
		uniq[c.CapacityType] = struct{}{}
	}
	keys := make([]string, 0, len(uniq))
	for k := range uniq {
		keys = append(keys, k)
	}
	return keys
}

func uniqueZones(available cloudprovider.Offerings) []string {
	uniq := map[string]struct{}{}
	for _, c := range available {
		uniq[c.Zone] = struct{}{}
	}
	keys := make([]string, 0, len(uniq))
	for k := range uniq {
		keys = append(keys, k)
	}
	return keys
}

func computeCapacity(ctx context.Context, it *cloudprovider.InstanceType) v1.ResourceList {
	resourceList := v1.ResourceList{
		v1.ResourceCPU:               *it.Capacity.Cpu(),
		v1.ResourceMemory:            *it.Capacity.Memory(),
		v1.ResourceEphemeralStorage:  resource.MustParse("20G"),
		v1.ResourcePods:              resource.MustParse("110"),
	}
	return resourceList
}

func lowerKabobCase(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", "-"))
}

// func nvidiaGPUs(info *ec2.InstanceTypeInfo) *resource.Quantity {
// 	count := int64(0)
// 	if info.GpuInfo != nil {
// 		for _, gpu := range info.GpuInfo.Gpus {
// 			if *gpu.Manufacturer == "NVIDIA" {
// 				count += *gpu.Count
// 			}
// 		}
// 	}
// 	return resources.Quantity(fmt.Sprint(count))
// }

// func amdGPUs(info *ec2.InstanceTypeInfo) *resource.Quantity {
// 	count := int64(0)
// 	if info.GpuInfo != nil {
// 		for _, gpu := range info.GpuInfo.Gpus {
// 			if *gpu.Manufacturer == "AMD" {
// 				count += *gpu.Count
// 			}
// 		}
// 	}
// 	return resources.Quantity(fmt.Sprint(count))
// }

// TODO: remove trn1 hardcode values once DescribeInstanceTypes contains the accelerator data
// Values found from: https://aws.amazon.com/ec2/instance-types/trn1/
// func awsNeurons(info *ec2.InstanceTypeInfo) *resource.Quantity {
// 	count := int64(0)
// 	if *info.InstanceType == "trn1.2xlarge" {
// 		count = int64(1)
// 	} else if *info.InstanceType == "trn1.32xlarge" {
// 		count = int64(16)
// 	} else if *info.InstanceType == "trn1n.32xlarge" {
// 		count = int64(16)
// 	} else if info.InferenceAcceleratorInfo != nil {
// 		for _, accelerator := range info.InferenceAcceleratorInfo.Accelerators {
// 			count += *accelerator.Count
// 		}
// 	}
// 	return resources.Quantity(fmt.Sprint(count))
// }

// func habanaGaudis(info *ec2.InstanceTypeInfo) *resource.Quantity {
// 	count := int64(0)
// 	if info.GpuInfo != nil {
// 		for _, gpu := range info.GpuInfo.Gpus {
// 			if *gpu.Manufacturer == "Habana" {
// 				count += *gpu.Count
// 			}
// 		}
// 	}
// 	return resources.Quantity(fmt.Sprint(count))
// }
