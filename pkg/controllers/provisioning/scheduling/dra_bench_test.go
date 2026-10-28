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

package scheduling

import (
	"context"
	"fmt"
	"testing"
	"unique"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

const (
	benchGPUModeKey = "example.com/bench-driver-mode-gpu"
	benchNICModeKey = "example.com/bench-driver-mode-nic"
)

// BenchmarkDRAAllocate measures one Allocate for a single-device claim through draNodeClaim, over a NodeClaim whose
// instance type options are a quarter GPU instance types. Each GPU instance type has 48 offerings (4 zones × 3 capacity
// types × 4 driver-mode combinations); every other instance type has 12. "scoped" GPU instance types have a GPU set
// and a NIC set scoped to their DRA modes; "unscoped" ones have a single unscoped GPU set, the shape before
// requirement-scoped sets. "cold" runs build fresh instance types before each Allocate (outside the timer), so the
// per-instance-type set availability is computed inside the measured Allocate, as in the first Allocate of a
// scheduling loop.
func BenchmarkDRAAllocate(b *testing.B) {
	v1.WellKnownLabels.Insert(benchGPUModeKey, benchNICModeKey)
	b.Cleanup(func() { v1.WellKnownLabels.Delete(benchGPUModeKey, benchNICModeKey) })
	for _, cold := range []bool{false, true} {
		for _, scoped := range []bool{false, true} {
			for _, n := range []int{100, 200, 400, 800} {
				name := fmt.Sprintf("%s/%s/instance-types=%d", map[bool]string{false: "warm", true: "cold"}[cold], map[bool]string{false: "unscoped", true: "scoped"}[scoped], n)
				b.Run(name, func(b *testing.B) { benchmarkDRAAllocate(b, n, scoped, cold) })
			}
		}
	}
}

func benchmarkDRAAllocate(b *testing.B, n int, scoped, cold bool) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}
	kubeClient := fakecr.NewClientBuilder().WithScheme(scheme).WithObjects(&resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu"},
		Spec: resourcev1.DeviceClassSpec{Selectors: []resourcev1.DeviceSelector{
			{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "gpu.example.com"`}},
		}},
	}).Build()
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "req",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "gpu", Count: 1},
		}}}},
	}
	buildInstanceTypes := func() cloudprovider.InstanceTypes {
		instanceTypes := make(cloudprovider.InstanceTypes, 0, n)
		for i := range n {
			instanceTypes = append(instanceTypes, benchInstanceType(fmt.Sprintf("it-%d", i), i%4 == 0, scoped))
		}
		return instanceTypes
	}
	nc := &NodeClaim{hostname: "bench"}
	nc.NodePoolName = "bench"
	nc.Requirements = scheduling.NewRequirements()
	nc.InstanceTypeOptions = buildInstanceTypes()

	ctx := context.Background()
	for b.Loop() {
		if cold {
			b.StopTimer()
			nc.InstanceTypeOptions = buildInstanceTypes()
			b.StartTimer()
		}
		alloc := dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, kubeClient, nil)
		if _, err := alloc.Allocate(ctx, &draNodeClaim{nc: nc}, []*resourcev1.ResourceClaim{claim}); err != nil {
			b.Fatal(err)
		}
	}
}

func benchInstanceType(name string, gpu, scoped bool) *cloudprovider.InstanceType {
	modes := []string{"device-plugin", "dynamic-resource-allocation"}
	if !gpu {
		modes = []string{""}
	}
	var offerings cloudprovider.Offerings
	for _, zone := range []string{"zone-a", "zone-b", "zone-c", "zone-d"} {
		for _, ct := range []string{v1.CapacityTypeSpot, v1.CapacityTypeOnDemand, v1.CapacityTypeReserved} {
			for _, gpuMode := range modes {
				for _, nicMode := range modes {
					reqs := scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
						scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, ct),
					)
					if gpu {
						reqs.Add(
							scheduling.NewRequirement(benchGPUModeKey, corev1.NodeSelectorOpIn, gpuMode),
							scheduling.NewRequirement(benchNICModeKey, corev1.NodeSelectorOpIn, nicMode),
						)
					}
					offerings = append(offerings, &cloudprovider.Offering{Requirements: reqs, Price: 1, Available: true})
				}
			}
		}
	}
	it := &cloudprovider.InstanceType{Name: name, Offerings: offerings}
	if !gpu {
		return it
	}
	gpus := &cloudprovider.ResourceSliceTemplate{Driver: unique.Make("gpu.example.com"), Pool: cloudprovider.ResourcePool{Name: unique.Make(name + "-gpus")}}
	for i := range 8 {
		gpus.Devices = append(gpus.Devices, cloudprovider.Device{Name: unique.Make(fmt.Sprintf("gpu-%d", i))})
	}
	if !scoped {
		it.DynamicResources = []*cloudprovider.DynamicResources{{ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{gpus}}}
		return it
	}
	nics := &cloudprovider.ResourceSliceTemplate{Driver: unique.Make("nic.example.com"), Pool: cloudprovider.ResourcePool{Name: unique.Make(name + "-nics")}}
	for i := range 4 {
		nics.Devices = append(nics.Devices, cloudprovider.Device{Name: unique.Make(fmt.Sprintf("nic-%d", i))})
	}
	draMode := func(key string) scheduling.Requirements {
		return scheduling.NewRequirements(scheduling.NewRequirement(key, corev1.NodeSelectorOpIn, "dynamic-resource-allocation"))
	}
	it.DynamicResources = []*cloudprovider.DynamicResources{
		{Requirements: draMode(benchGPUModeKey), ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{gpus}},
		{Requirements: draMode(benchNICModeKey), ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{nics}},
	}
	return it
}
