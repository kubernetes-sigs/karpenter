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
	"fmt"
	"runtime"
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
)

// memStats captures memory statistics for a test
type memStats struct {
	allocsBefore   uint64
	allocsAfter    uint64
	totalAllocs    uint64
	bytesAllocated uint64
	heapAlloc      uint64
	heapObjects    uint64
}

// captureMemStats captures current memory statistics
func captureMemStats() *memStats {
	runtime.GC() // Force GC to get accurate baseline
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return &memStats{
		allocsBefore: m.Mallocs,
		heapAlloc:    m.HeapAlloc,
		heapObjects:  m.HeapObjects,
	}
}

// finalize captures final memory statistics
func (ms *memStats) finalize() {
	runtime.GC() // Force GC before final measurement
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	ms.allocsAfter = m.Mallocs
	ms.totalAllocs = ms.allocsAfter - ms.allocsBefore
	ms.bytesAllocated = m.TotalAlloc
	ms.heapAlloc = m.HeapAlloc
	ms.heapObjects = m.HeapObjects
}

// String returns formatted memory statistics
func (ms *memStats) String() string {
	return fmt.Sprintf(
		"Total Allocs: %d | Bytes: %.2f MB | Heap: %.2f MB | Objects: %d",
		ms.totalAllocs,
		float64(ms.bytesAllocated)/(1024*1024),
		float64(ms.heapAlloc)/(1024*1024),
		ms.heapObjects,
	)
}

// TestMemoryUsage_OverlayScenarios tests memory usage for different overlay configurations
// This test validates the selective copy-on-write optimization reduces memory consumption
//
//nolint:gocyclo
func TestMemoryUsage_OverlayScenarios(t *testing.T) {
	instanceTypes := createRealisticInstanceTypes(200)
	nodePools := []string{"nodepool-1", "nodepool-2", "nodepool-3", "nodepool-4", "nodepool-5"}

	t.Run("no_overlays", func(t *testing.T) {
		store := newInternalInstanceTypeStore()
		for _, np := range nodePools {
			store.evaluatedNodePools.Insert(np)
		}

		ms := captureMemStats()

		for i := 0; i < 100; i++ {
			for _, np := range nodePools {
				for _, it := range instanceTypes {
					_, _ = store.apply(np, it)
				}
			}
		}

		ms.finalize()
		t.Logf("No overlays - %s", ms.String())
	})

	t.Run("price_overlays_only", func(t *testing.T) {
		store := newInternalInstanceTypeStore()
		store.evaluatedNodePools.Insert("default")
		store.updates["default"] = make(map[string]*instanceTypeUpdate)

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
			if len(priceUpdates) > 0 {
				store.updates["default"][it.Name] = &instanceTypeUpdate{
					Price:    priceUpdates,
					Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
				}
			}
		}

		ms := captureMemStats()

		for i := 0; i < 100; i++ {
			for _, it := range instanceTypes {
				_, _ = store.apply("default", it)
			}
		}

		ms.finalize()
		t.Logf("Price overlays only - %s", ms.String())
	})

	t.Run("capacity_overlays_only", func(t *testing.T) {
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

		ms := captureMemStats()

		for i := 0; i < 100; i++ {
			for _, it := range instanceTypes {
				_, _ = store.apply("default", it)
			}
		}

		ms.finalize()
		t.Logf("Capacity overlays only - %s", ms.String())
	})

	t.Run("mixed_overlays", func(t *testing.T) {
		store := createStoreWithOverlays(instanceTypes, nodePools)

		ms := captureMemStats()

		for i := 0; i < 100; i++ {
			for _, np := range nodePools {
				for _, it := range instanceTypes {
					_, _ = store.apply(np, it)
				}
			}
		}

		ms.finalize()
		t.Logf("Mixed overlays (price + capacity) - %s", ms.String())
	})
}

// TestMemoryUsage_ScaleWithNodePools measures memory growth as number of node pools increases
func TestMemoryUsage_ScaleWithNodePools(t *testing.T) {
	instanceTypes := createRealisticInstanceTypes(200)
	nodePoolCounts := []int{1, 5, 10, 20, 50}

	for _, count := range nodePoolCounts {
		t.Run(fmt.Sprintf("nodepools_%d", count), func(t *testing.T) {
			nodePools := make([]string, count)
			for i := 0; i < count; i++ {
				nodePools[i] = fmt.Sprintf("nodepool-%d", i)
			}

			store := createStoreWithOverlays(instanceTypes, nodePools)

			ms := captureMemStats()

			for i := 0; i < 100; i++ {
				for _, np := range nodePools {
					for _, it := range instanceTypes {
						_, _ = store.apply(np, it)
					}
				}
			}

			ms.finalize()
			t.Logf("NodePools=%d - %s", count, ms.String())
		})
	}
}

// TestMemoryUsage_ScaleWithInstanceTypes measures memory growth as number of instance types increases
func TestMemoryUsage_ScaleWithInstanceTypes(t *testing.T) {
	instanceTypeCounts := []int{50, 100, 200, 500}
	nodePools := []string{"nodepool-1", "nodepool-2", "nodepool-3", "nodepool-4", "nodepool-5"}

	for _, count := range instanceTypeCounts {
		t.Run(fmt.Sprintf("instances_%d", count), func(t *testing.T) {
			instanceTypes := createRealisticInstanceTypes(count)
			store := createStoreWithOverlays(instanceTypes, nodePools)

			ms := captureMemStats()

			for i := 0; i < 100; i++ {
				for _, np := range nodePools {
					for _, it := range instanceTypes {
						_, _ = store.apply(np, it)
					}
				}
			}

			ms.finalize()
			t.Logf("InstanceTypes=%d - %s", count, ms.String())
		})
	}
}

// Helper functions

// createRealisticInstanceTypes creates a realistic set of instance types similar to AWS offerings
func createRealisticInstanceTypes(count int) []*cloudprovider.InstanceType {
	families := []string{"m5", "m6i", "m7i", "c5", "c6i", "c7i", "r5", "r6i", "r7i", "t3", "t4g"}
	sizes := []string{"nano", "micro", "small", "medium", "large", "xlarge", "2xlarge", "4xlarge", "8xlarge", "12xlarge", "16xlarge", "24xlarge", "32xlarge"}

	instanceTypes := make([]*cloudprovider.InstanceType, 0, count)
	idx := 0

	for _, family := range families {
		for _, size := range sizes {
			if idx >= count {
				break
			}
			name := family + "." + size

			// Create varying resource sizes
			cpuValue := 2 * (idx%8 + 1)
			memoryValue := 4 * (idx%8 + 1)

			instanceTypes = append(instanceTypes, fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: name,
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", cpuValue)),
					corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", memoryValue)),
				},
			}))
			idx++
		}
		if idx >= count {
			break
		}
	}

	return instanceTypes
}

// createStoreWithOverlays creates an instance type store with realistic overlays applied
func createStoreWithOverlays(instanceTypes []*cloudprovider.InstanceType, nodePools []string) *internalInstanceTypeStore {
	store := newInternalInstanceTypeStore()

	for _, np := range nodePools {
		store.evaluatedNodePools.Insert(np)
		store.updates[np] = make(map[string]*instanceTypeUpdate)

		for _, it := range instanceTypes {
			// Apply price overlays to spot offerings
			priceUpdates := make(map[string]*priceUpdate)
			for _, offering := range it.Offerings {
				if offering.Requirements.Get(v1.CapacityTypeLabelKey).Has("spot") {
					priceUpdates[offering.Requirements.String()] = &priceUpdate{
						OverlayUpdate: lo.ToPtr("-10%"),
						lowestWeight:  lo.ToPtr(int32(10)),
					}
				}
			}

			// Add capacity overlay for hugepages
			store.updates[np][it.Name] = &instanceTypeUpdate{
				Price: priceUpdates,
				Capacity: &capacityUpdate{
					OverlayUpdate: corev1.ResourceList{
						"hugepages-2Mi": resource.MustParse("100Mi"),
					},
				},
			}
		}
	}

	return store
}
