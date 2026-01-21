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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
)

// Memory limits for overlay scenarios (in number of allocations)
// These limits are derived from baseline measurements and include a 20% buffer
// to account for natural variance between test runs.
// If these tests fail, it indicates a potential memory regression that should be investigated.
const (
	// MaxAllocsNoOverlays is the maximum allowed allocations when no overlays are applied.
	// With no overlays, instance types should be returned with minimal copying.
	// Baseline: ~72,000 allocations
	MaxAllocsNoOverlays = 100000

	// MaxAllocsPriceOverlaysOnly is the maximum allowed allocations for price-only overlays.
	// Price overlays require copying the offerings slice but not capacity.
	// Baseline: ~1,380,000 allocations
	MaxAllocsPriceOverlaysOnly = 1700000

	// MaxAllocsCapacityOverlaysOnly is the maximum allowed allocations for capacity-only overlays.
	// Capacity overlays require copying the capacity map but not offerings.
	// Baseline: ~72,000 allocations
	MaxAllocsCapacityOverlaysOnly = 100000

	// MaxAllocsMixedOverlays is the maximum allowed allocations when both price and capacity overlays are applied.
	// This is the most expensive scenario as both offerings and capacity need to be copied.
	// Baseline: ~7,260,000 allocations (200 instance types x 5 node pools x 100 iterations)
	MaxAllocsMixedOverlays = 8800000

	// MaxAllocsPerNodePool is the maximum allowed allocations per node pool when scaling.
	// Used to verify memory scales linearly with node pool count.
	// Baseline: ~1,451,000 allocations per node pool
	MaxAllocsPerNodePool = 1500000

	// MaxAllocsPerInstanceType is the maximum allowed allocations per instance type when scaling.
	// Used to verify memory scales linearly with instance type count.
	// Baseline: ~10,150 per instance type per node pool
	MaxAllocsPerInstanceType = 10500
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

var _ = Describe("Memory Usage Overlay Scenarios", func() {
	var instanceTypes []*cloudprovider.InstanceType
	var nodePools []string

	BeforeEach(func() {
		instanceTypes = createRealisticInstanceTypes(200)
		nodePools = []string{"nodepool-1", "nodepool-2", "nodepool-3", "nodepool-4", "nodepool-5"}
	})

	Context("no overlays", func() {
		It("should have minimal allocations when no overlays are applied", func() {
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
			GinkgoWriter.Printf("No overlays - %s\n", ms.String())

			// Verify allocations are within acceptable limits
			Expect(ms.totalAllocs).To(BeNumerically("<=", MaxAllocsNoOverlays),
				"memory allocations exceeded limit for no overlays scenario: got %d, max %d", ms.totalAllocs, MaxAllocsNoOverlays)
		})
	})

	Context("price overlays only", func() {
		It("should have controlled allocations with price-only overlays", func() {
			store := newInternalInstanceTypeStore()
			store.evaluatedNodePools.Insert("default")

			updates := make(map[string]map[string]*instanceTypeUpdate)
			updates["default"] = make(map[string]*instanceTypeUpdate)

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
					updates["default"][it.Name] = &instanceTypeUpdate{
						Price:    priceUpdates,
						Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
					}
				}
			}
			store.updates = updates

			ms := captureMemStats()

			for i := 0; i < 100; i++ {
				for _, it := range instanceTypes {
					_, _ = store.apply("default", it)
				}
			}

			ms.finalize()
			GinkgoWriter.Printf("Price overlays only - %s\n", ms.String())

			// Verify allocations are within acceptable limits
			Expect(ms.totalAllocs).To(BeNumerically("<=", MaxAllocsPriceOverlaysOnly),
				"memory allocations exceeded limit for price overlays only scenario: got %d, max %d", ms.totalAllocs, MaxAllocsPriceOverlaysOnly)
		})
	})

	Context("capacity overlays only", func() {
		It("should have controlled allocations with capacity-only overlays", func() {
			store := newInternalInstanceTypeStore()
			store.evaluatedNodePools.Insert("default")

			updates := make(map[string]map[string]*instanceTypeUpdate)
			updates["default"] = make(map[string]*instanceTypeUpdate)

			for _, it := range instanceTypes {
				updates["default"][it.Name] = &instanceTypeUpdate{
					Price: nil,
					Capacity: &capacityUpdate{
						OverlayUpdate: corev1.ResourceList{
							"hugepages-2Mi": resource.MustParse("100Mi"),
						},
					},
				}
			}
			store.updates = updates

			ms := captureMemStats()

			for i := 0; i < 100; i++ {
				for _, it := range instanceTypes {
					_, _ = store.apply("default", it)
				}
			}

			ms.finalize()
			GinkgoWriter.Printf("Capacity overlays only - %s\n", ms.String())

			// Verify allocations are within acceptable limits
			Expect(ms.totalAllocs).To(BeNumerically("<=", MaxAllocsCapacityOverlaysOnly),
				"memory allocations exceeded limit for capacity overlays only scenario: got %d, max %d", ms.totalAllocs, MaxAllocsCapacityOverlaysOnly)
		})
	})

	Context("mixed overlays", func() {
		It("should have controlled allocations with both price and capacity overlays", func() {
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
			GinkgoWriter.Printf("Mixed overlays (price + capacity) - %s\n", ms.String())

			// Verify allocations are within acceptable limits
			Expect(ms.totalAllocs).To(BeNumerically("<=", MaxAllocsMixedOverlays),
				"memory allocations exceeded limit for mixed overlays scenario: got %d, max %d", ms.totalAllocs, MaxAllocsMixedOverlays)
		})
	})
})

var _ = Describe("Memory Usage Scale With NodePools", func() {
	var instanceTypes []*cloudprovider.InstanceType

	BeforeEach(func() {
		instanceTypes = createRealisticInstanceTypes(200)
	})

	DescribeTable("should scale linearly with node pool count",
		func(count int) {
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
			GinkgoWriter.Printf("NodePools=%d - %s\n", count, ms.String())

			// Verify memory scales linearly with node pool count
			// Allow for some overhead, but allocations should be roughly proportional
			maxExpectedAllocs := uint64(count) * MaxAllocsPerNodePool //#nosec G115 -- count is always positive from test table
			Expect(ms.totalAllocs).To(BeNumerically("<=", maxExpectedAllocs),
				"memory allocations exceeded limit for %d node pools: got %d, max %d", count, ms.totalAllocs, maxExpectedAllocs)
		},
		Entry("1 nodepool", 1),
		Entry("5 nodepools", 5),
		Entry("10 nodepools", 10),
		Entry("20 nodepools", 20),
		Entry("50 nodepools", 50),
	)
})

var _ = Describe("Memory Usage Scale With InstanceTypes", func() {
	var nodePools []string

	BeforeEach(func() {
		nodePools = []string{"nodepool-1", "nodepool-2", "nodepool-3", "nodepool-4", "nodepool-5"}
	})

	DescribeTable("should scale linearly with instance type count",
		func(count int) {
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
			GinkgoWriter.Printf("InstanceTypes=%d - %s\n", count, ms.String())

			// Verify memory scales linearly with instance type count
			// Factor in the number of node pools and iterations
			maxExpectedAllocs := uint64(count) * uint64(len(nodePools)) * MaxAllocsPerInstanceType //#nosec G115 -- count and len(nodePools) are always positive
			Expect(ms.totalAllocs).To(BeNumerically("<=", maxExpectedAllocs),
				"memory allocations exceeded limit for %d instance types: got %d, max %d", count, ms.totalAllocs, maxExpectedAllocs)
		},
		Entry("50 instance types", 50),
		Entry("100 instance types", 100),
		Entry("200 instance types", 200),
		Entry("500 instance types", 500),
	)
})

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

	updates := make(map[string]map[string]*instanceTypeUpdate)

	for _, np := range nodePools {
		store.evaluatedNodePools.Insert(np)
		updates[np] = make(map[string]*instanceTypeUpdate)

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
			updates[np][it.Name] = &instanceTypeUpdate{
				Price: priceUpdates,
				Capacity: &capacityUpdate{
					OverlayUpdate: corev1.ResourceList{
						"hugepages-2Mi": resource.MustParse("100Mi"),
					},
				},
			}
		}
	}

	store.updates = updates
	return store
}
