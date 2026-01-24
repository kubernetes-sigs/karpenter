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

package nodeoverlay

import (
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
)

// BenchmarkStoreApply benchmarks the memory usage of applying overlays to instance types.
// This benchmark helps validate that the selective copy-on-write strategy significantly
// reduces memory allocations compared to deep copying.
func BenchmarkStoreApply(b *testing.B) {
	// Create a realistic set of instance types similar to AWS offerings
	instanceTypes := make([]*cloudprovider.InstanceType, 0, 200)
	families := []string{"m5", "m6i", "m7i", "c5", "c6i", "c7i", "r5", "r6i", "r7i"}
	sizes := []string{"large", "xlarge", "2xlarge", "4xlarge", "8xlarge", "12xlarge", "16xlarge", "24xlarge", "32xlarge"}

	for _, family := range families {
		for _, size := range sizes {
			name := family + "." + size
			instanceTypes = append(instanceTypes, fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: name,
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
			}))
		}
	}

	// Create store with overlays applied
	store := newInternalInstanceTypeStore()
	store.evaluatedNodePools.Insert("default")
	store.updates["default"] = make(map[string]*instanceTypeUpdate)

	// Apply price overlays to spot offerings
	for _, it := range instanceTypes {
		priceUpdates := make(map[string]*priceUpdate)
		for _, offering := range it.Offerings {
			if offering.Requirements.Get(v1.CapacityTypeLabelKey).Has("spot") {
				priceUpdates[offering.Requirements.String()] = &priceUpdate{
					OverlayUpdate: lo.ToPtr("-10%"),
					lowestWeight:  lo.ToPtr(int32(10)),
				}
			}
		}
		store.updates["default"][it.Name] = &instanceTypeUpdate{
			Price: priceUpdates,
			Capacity: &capacityUpdate{
				OverlayUpdate: corev1.ResourceList{
					"hugepages-2Mi": resource.MustParse("100Mi"),
				},
			},
		}
	}

	b.Run("selective-copy-apply", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, it := range instanceTypes {
				_, _ = store.apply("default", it)
			}
		}
	})
}

// BenchmarkStoreApplyNoOverlay benchmarks applying with no overlays (everything shared)
func BenchmarkStoreApplyNoOverlay(b *testing.B) {
	instanceTypes := make([]*cloudprovider.InstanceType, 0, 200)
	for i := 0; i < 200; i++ {
		instanceTypes = append(instanceTypes, fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "m5.large",
		}))
	}

	store := newInternalInstanceTypeStore()
	store.evaluatedNodePools.Insert("default")
	// No updates - everything will be shared

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, it := range instanceTypes {
			_, _ = store.apply("default", it)
		}
	}
}

// BenchmarkStoreApplyPriceOnly benchmarks applying with only price overlays
func BenchmarkStoreApplyPriceOnly(b *testing.B) {
	instanceTypes := make([]*cloudprovider.InstanceType, 0, 200)
	for i := 0; i < 200; i++ {
		instanceTypes = append(instanceTypes, fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "m5.large",
		}))
	}

	store := newInternalInstanceTypeStore()
	store.evaluatedNodePools.Insert("default")
	store.updates["default"] = make(map[string]*instanceTypeUpdate)

	for _, it := range instanceTypes {
		priceUpdates := make(map[string]*priceUpdate)
		for _, offering := range it.Offerings {
			priceUpdates[offering.Requirements.String()] = &priceUpdate{
				OverlayUpdate: lo.ToPtr("+0.01"),
				lowestWeight:  lo.ToPtr(int32(10)),
			}
		}
		store.updates["default"][it.Name] = &instanceTypeUpdate{
			Price:    priceUpdates,
			Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, it := range instanceTypes {
			_, _ = store.apply("default", it)
		}
	}
}

// BenchmarkStoreApplyCapacityOnly benchmarks applying with only capacity overlays
func BenchmarkStoreApplyCapacityOnly(b *testing.B) {
	instanceTypes := make([]*cloudprovider.InstanceType, 0, 200)
	for i := 0; i < 200; i++ {
		instanceTypes = append(instanceTypes, fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "m5.large",
		}))
	}

	store := newInternalInstanceTypeStore()
	store.evaluatedNodePools.Insert("default")
	store.updates["default"] = make(map[string]*instanceTypeUpdate)

	for _, it := range instanceTypes {
		store.updates["default"][it.Name] = &instanceTypeUpdate{
			Price: nil,
			Capacity: &capacityUpdate{
				OverlayUpdate: corev1.ResourceList{
					"hugepages-2Mi": resource.MustParse("100Mi"),
				},
			},
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, it := range instanceTypes {
			_, _ = store.apply("default", it)
		}
	}
}

// BenchmarkStoreApplyBothOverlays benchmarks applying with both price and capacity overlays
func BenchmarkStoreApplyBothOverlays(b *testing.B) {
	instanceTypes := make([]*cloudprovider.InstanceType, 0, 200)
	for i := 0; i < 200; i++ {
		instanceTypes = append(instanceTypes, fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "m5.large",
		}))
	}

	store := newInternalInstanceTypeStore()
	store.evaluatedNodePools.Insert("default")
	store.updates["default"] = make(map[string]*instanceTypeUpdate)

	for _, it := range instanceTypes {
		priceUpdates := make(map[string]*priceUpdate)
		for _, offering := range it.Offerings {
			priceUpdates[offering.Requirements.String()] = &priceUpdate{
				OverlayUpdate: lo.ToPtr("+0.01"),
				lowestWeight:  lo.ToPtr(int32(10)),
			}
		}
		store.updates["default"][it.Name] = &instanceTypeUpdate{
			Price: priceUpdates,
			Capacity: &capacityUpdate{
				OverlayUpdate: corev1.ResourceList{
					"hugepages-2Mi": resource.MustParse("100Mi"),
				},
			},
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, it := range instanceTypes {
			_, _ = store.apply("default", it)
		}
	}
}

// BenchmarkStoreApplyAllScenario benchmarks the full ApplyAll method with multiple node pools
func BenchmarkStoreApplyAllScenario(b *testing.B) {
	// Simulate 5 node pools with 200 instance types each
	instanceTypes := make([]*cloudprovider.InstanceType, 0, 200)
	families := []string{"m5", "m6i", "c5", "c6i", "r5", "r6i"}
	sizes := []string{"large", "xlarge", "2xlarge", "4xlarge", "8xlarge", "16xlarge", "32xlarge"}

	for _, family := range families {
		for _, size := range sizes {
			for i := 0; i < 5; i++ { // Create multiple instances to reach ~200
				name := family + "." + size
				instanceTypes = append(instanceTypes, fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: name,
				}))
			}
		}
	}

	store := NewInstanceTypeStore()
	internalStore := newInternalInstanceTypeStore()

	// Setup overlays for 5 node pools
	nodePools := []string{"nodepool-1", "nodepool-2", "nodepool-3", "nodepool-4", "nodepool-5"}
	for _, np := range nodePools {
		internalStore.evaluatedNodePools.Insert(np)
		internalStore.updates[np] = make(map[string]*instanceTypeUpdate)

		for _, it := range instanceTypes {
			priceUpdates := make(map[string]*priceUpdate)
			for _, offering := range it.Offerings {
				priceUpdates[offering.Requirements.String()] = &priceUpdate{
					OverlayUpdate: lo.ToPtr("-5%"),
					lowestWeight:  lo.ToPtr(int32(10)),
				}
			}
			internalStore.updates[np][it.Name] = &instanceTypeUpdate{
				Price: priceUpdates,
				Capacity: &capacityUpdate{
					OverlayUpdate: corev1.ResourceList{
						"hugepages-2Mi": resource.MustParse("100Mi"),
					},
				},
			}
		}
	}

	store.UpdateStore(internalStore)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, np := range nodePools {
			_, _ = store.ApplyAll(np, instanceTypes)
		}
	}
}

// setupNodeOverlayBenchmarkStore creates a store with realistic controller scenario data
func setupNodeOverlayBenchmarkStore(instanceTypes []*cloudprovider.InstanceType, nodePools []string) *internalInstanceTypeStore {
	overlay := v1alpha1.NodeOverlay{
		Spec: v1alpha1.NodeOverlaySpec{
			Weight: lo.ToPtr(int32(100)),
		},
	}

	store := newInternalInstanceTypeStore()

	for _, np := range nodePools {
		store.evaluatedNodePools.Insert(np)
		store.updates[np] = make(map[string]*instanceTypeUpdate)

		for _, it := range instanceTypes {
			// Apply overlay to spot offerings only
			spotOfferings := cloudprovider.Offerings{}
			for _, offering := range it.Offerings {
				if offering.Requirements.Get(v1.CapacityTypeLabelKey).Has("spot") {
					spotOfferings = append(spotOfferings, offering)
				}
			}
			if len(spotOfferings) > 0 {
				store.updateInstanceTypeOffering(np, it.Name, overlay, spotOfferings)
			}
		}
	}

	return store
}

// BenchmarkNodeOverlayControllerScenario benchmarks a realistic controller scenario
//
//nolint:gocyclo
func BenchmarkNodeOverlayControllerScenario(b *testing.B) {
	// Simulate the scenario from GitHub issue #2655:
	// - 200 instance types (m/r/c families, generations 6-7, large to 32xlarge)
	// - 5 node pools
	// - Spot capacity type filter overlay applied
	instanceTypes := make([]*cloudprovider.InstanceType, 0, 200)
	families := []string{"m6i", "m7i", "r6i", "r7i", "c6i", "c7i"}
	sizes := []string{"large", "xlarge", "2xlarge", "4xlarge", "8xlarge", "12xlarge", "16xlarge", "24xlarge", "32xlarge"}

	for _, family := range families {
		for _, size := range sizes {
			for i := 0; i < 4; i++ { // ~216 instance types total
				name := family + "." + size
				instanceTypes = append(instanceTypes, fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: name,
				}))
			}
		}
	}

	nodePools := []string{"nodepool-1", "nodepool-2", "nodepool-3", "nodepool-4", "nodepool-5"}
	store := setupNodeOverlayBenchmarkStore(instanceTypes, nodePools)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, np := range nodePools {
			for _, it := range instanceTypes {
				_, _ = store.apply(np, it)
			}
		}
	}
}
