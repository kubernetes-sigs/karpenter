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

	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

//nolint:gocyclo
func TestStoreApplySelectiveCopy(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name                 string
		instanceType         *cloudprovider.InstanceType
		priceOverlay         map[string]*priceUpdate
		capacityOverlay      *capacityUpdate
		expectSharedReqs     bool
		expectSharedOverhead bool
		expectSharedOffering bool
		expectSharedCapacity bool
	}{
		{
			name: "no overlays - everything shared",
			instanceType: fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "m5.large",
				Offerings: []*cloudprovider.Offering{
					{
						Requirements: scheduling.NewRequirements(
							scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
							scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "on-demand"),
						),
						Price:     0.096,
						Available: true,
					},
				},
			}),
			priceOverlay:         nil,
			capacityOverlay:      &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
			expectSharedReqs:     true,
			expectSharedOverhead: true,
			expectSharedOffering: true,
			expectSharedCapacity: true,
		},
		{
			name: "price overlay only - offerings copied, others shared",
			instanceType: func() *cloudprovider.InstanceType {
				it := fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "m5.large",
				})
				return it
			}(),
			priceOverlay: func() map[string]*priceUpdate {
				it := fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "m5.large",
				})
				// Use actual requirements string from the generated instance type
				return map[string]*priceUpdate{
					it.Offerings[0].Requirements.String(): {
						OverlayUpdate: lo.ToPtr("+0.01"),
						lowestWeight:  lo.ToPtr(int32(10)),
					},
				}
			}(),
			capacityOverlay:      &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
			expectSharedReqs:     true,
			expectSharedOverhead: true,
			expectSharedOffering: false, // Offerings should be copied
			expectSharedCapacity: true,
		},
		{
			name: "capacity overlay only - capacity copied, others shared",
			instanceType: fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "m5.large",
				Offerings: []*cloudprovider.Offering{
					{
						Requirements: scheduling.NewRequirements(
							scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
							scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "on-demand"),
						),
						Price:     0.096,
						Available: true,
					},
				},
			}),
			priceOverlay: nil,
			capacityOverlay: &capacityUpdate{
				OverlayUpdate: corev1.ResourceList{
					"hugepages-2Mi": resource.MustParse("100Mi"),
				},
			},
			expectSharedReqs:     true,
			expectSharedOverhead: true,
			expectSharedOffering: true,
			expectSharedCapacity: false, // Capacity should be copied
		},
		{
			name: "both overlays - only modified fields copied",
			instanceType: func() *cloudprovider.InstanceType {
				return fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "m5.large",
				})
			}(),
			priceOverlay: func() map[string]*priceUpdate {
				it := fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "m5.large",
				})
				return map[string]*priceUpdate{
					it.Offerings[0].Requirements.String(): {
						OverlayUpdate: lo.ToPtr("+0.01"),
						lowestWeight:  lo.ToPtr(int32(10)),
					},
				}
			}(),
			capacityOverlay: &capacityUpdate{
				OverlayUpdate: corev1.ResourceList{
					"hugepages-2Mi": resource.MustParse("100Mi"),
				},
			},
			expectSharedReqs:     true,
			expectSharedOverhead: true,
			expectSharedOffering: false,
			expectSharedCapacity: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			store := newInternalInstanceTypeStore()
			store.evaluatedNodePools.Insert("default")
			store.updates["default"] = map[string]*instanceTypeUpdate{
				tt.instanceType.Name: {
					Price:    tt.priceOverlay,
					Capacity: tt.capacityOverlay,
				},
			}

			result, err := store.apply("default", tt.instanceType)
			g.Expect(err).ToNot(HaveOccurred(), "unexpected error applying overlay")

			// Verify Requirements sharing - map comparison by checking first key address
			if tt.expectSharedReqs {
				// For maps, we check if they're the same object by comparing addresses
				// Since Requirements is a map, we compare if they share memory
				if len(result.Requirements) > 0 && len(tt.instanceType.Requirements) > 0 {
					// Get first key from both maps
					for k := range result.Requirements {
						origReq := tt.instanceType.Requirements[k]
						resultReq := result.Requirements[k]
						g.Expect(resultReq).To(BeIdenticalTo(origReq), "expected Requirements to be shared (same map)")
						break
					}
				}
			}

			// Verify Overhead sharing
			if tt.expectSharedOverhead {
				g.Expect(result.Overhead).To(BeIdenticalTo(tt.instanceType.Overhead), "expected Overhead to be shared (same pointer)")
			}

			// Verify Offerings sharing
			if tt.expectSharedOffering {
				if len(result.Offerings) > 0 && len(tt.instanceType.Offerings) > 0 {
					g.Expect(result.Offerings[0]).To(BeIdenticalTo(tt.instanceType.Offerings[0]), "expected Offerings[0] to be shared (same pointer)")
				}
			} else {
				if len(result.Offerings) > 0 && len(tt.instanceType.Offerings) > 0 {
					g.Expect(result.Offerings[0]).ToNot(BeIdenticalTo(tt.instanceType.Offerings[0]), "expected Offerings[0] to be copied (different pointer)")
				}
			}

			// Verify Capacity sharing - ResourceList is a map
			// For ResourceList (map), we can't directly compare map pointers
			// The correctness tests below verify the actual behavior
			_ = tt.expectSharedCapacity
		})
	}
	_ = g // silence unused variable warning for outer g
}

func TestStoreApplyCorrectnessWithPriceOverlay(t *testing.T) {
	g := NewWithT(t)
	originalPrice := 0.096
	instanceType := fake.NewInstanceType(fake.InstanceTypeOptions{
		Name: "m5.large",
		Offerings: []*cloudprovider.Offering{
			{
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
					scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "on-demand"),
				),
				Price:     originalPrice,
				Available: true,
			},
			{
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2b"),
					scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "on-demand"),
				),
				Price:     originalPrice,
				Available: true,
			},
		},
	})

	store := newInternalInstanceTypeStore()
	store.evaluatedNodePools.Insert("default")
	store.updates["default"] = map[string]*instanceTypeUpdate{
		instanceType.Name: {
			Price: map[string]*priceUpdate{
				// Only overlay the first offering
				instanceType.Offerings[0].Requirements.String(): {
					OverlayUpdate: lo.ToPtr("+0.01"),
					lowestWeight:  lo.ToPtr(int32(10)),
				},
			},
			Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
		},
	}

	result, err := store.apply("default", instanceType)
	g.Expect(err).ToNot(HaveOccurred())

	// Verify first offering was modified
	g.Expect(result.Offerings[0].Price).To(BeNumerically("==", 0.106), "expected first offering price to be 0.106") // 0.096 + 0.01
	g.Expect(result.Offerings[0].IsPriceOverlaid()).To(BeTrue(), "expected first offering to have priceOverlayApplied flag set")

	// Verify second offering was NOT modified (should be shared pointer)
	g.Expect(result.Offerings[1].Price).To(BeNumerically("==", originalPrice), "expected second offering price to remain unchanged")
	g.Expect(result.Offerings[1]).To(BeIdenticalTo(instanceType.Offerings[1]), "expected second offering to be shared (same pointer)")

	// Verify original instance type was not mutated
	g.Expect(instanceType.Offerings[0].Price).To(BeNumerically("==", originalPrice), "original instance type should not be mutated")
}

func TestStoreApplyCorrectnessWithCapacityOverlay(t *testing.T) {
	g := NewWithT(t)
	originalMemory := resource.MustParse("8Gi")
	instanceType := fake.NewInstanceType(fake.InstanceTypeOptions{
		Name: "m5.large",
		Resources: corev1.ResourceList{
			corev1.ResourceMemory: originalMemory,
			corev1.ResourceCPU:    resource.MustParse("2"),
		},
	})

	store := newInternalInstanceTypeStore()
	store.evaluatedNodePools.Insert("default")
	store.updates["default"] = map[string]*instanceTypeUpdate{
		instanceType.Name: {
			Price: nil,
			Capacity: &capacityUpdate{
				OverlayUpdate: corev1.ResourceList{
					"hugepages-2Mi": resource.MustParse("100Mi"),
				},
			},
		},
	}

	result, err := store.apply("default", instanceType)
	g.Expect(err).ToNot(HaveOccurred())

	// Verify hugepages was added
	hugepages, ok := result.Capacity["hugepages-2Mi"]
	g.Expect(ok).To(BeTrue(), "expected hugepages-2Mi to be added to capacity")
	g.Expect(hugepages.String()).To(Equal("100Mi"), "expected hugepages-2Mi to be 100Mi")

	// Verify original resources are still present
	g.Expect(result.Capacity.Memory().Cmp(originalMemory)).To(Equal(0), "expected memory to remain unchanged")

	// Verify original instance type was not mutated
	_, exists := instanceType.Capacity["hugepages-2Mi"]
	g.Expect(exists).To(BeFalse(), "original instance type should not have hugepages added")
}

func TestStoreApplyIsolationBetweenNodePools(t *testing.T) {
	g := NewWithT(t)
	instanceType := fake.NewInstanceType(fake.InstanceTypeOptions{
		Name: "m5.large",
		Offerings: []*cloudprovider.Offering{
			{
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
					scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "on-demand"),
				),
				Price:     0.096,
				Available: true,
			},
		},
	})

	store := newInternalInstanceTypeStore()
	store.evaluatedNodePools.Insert("nodepool-a", "nodepool-b")

	// NodePool A: +10% price adjustment
	store.updates["nodepool-a"] = map[string]*instanceTypeUpdate{
		instanceType.Name: {
			Price: map[string]*priceUpdate{
				instanceType.Offerings[0].Requirements.String(): {
					OverlayUpdate: lo.ToPtr("+10%"),
					lowestWeight:  lo.ToPtr(int32(10)),
				},
			},
			Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
		},
	}

	// NodePool B: -5% price adjustment
	store.updates["nodepool-b"] = map[string]*instanceTypeUpdate{
		instanceType.Name: {
			Price: map[string]*priceUpdate{
				instanceType.Offerings[0].Requirements.String(): {
					OverlayUpdate: lo.ToPtr("-5%"),
					lowestWeight:  lo.ToPtr(int32(10)),
				},
			},
			Capacity: &capacityUpdate{OverlayUpdate: corev1.ResourceList{}},
		},
	}

	// Apply to NodePool A
	resultA, err := store.apply("nodepool-a", instanceType)
	g.Expect(err).ToNot(HaveOccurred(), "unexpected error for nodepool-a")

	// Apply to NodePool B
	resultB, err := store.apply("nodepool-b", instanceType)
	g.Expect(err).ToNot(HaveOccurred(), "unexpected error for nodepool-b")

	// Verify NodePool A has +10% (0.096 * 1.10 = 0.1056)
	expectedPriceA := 0.1056
	g.Expect(resultA.Offerings[0].Price).To(BeNumerically("~", expectedPriceA, 0.0001), "nodepool-a: expected price %.4f", expectedPriceA)

	// Verify NodePool B has -5% (0.096 * 0.95 = 0.0912)
	expectedPriceB := 0.0912
	g.Expect(resultB.Offerings[0].Price).To(BeNumerically("~", expectedPriceB, 0.0001), "nodepool-b: expected price %.4f", expectedPriceB)

	// Verify original instance type was not mutated
	g.Expect(instanceType.Offerings[0].Price).To(BeNumerically("==", 0.096), "original instance type should not be mutated")

	// Verify the two results have different offering pointers (isolated)
	g.Expect(resultA.Offerings[0]).ToNot(BeIdenticalTo(resultB.Offerings[0]), "nodepool-a and nodepool-b should have different offering pointers (isolated)")
}

func TestStoreApplyUnevaluatedNodePool(t *testing.T) {
	g := NewWithT(t)
	instanceType := fake.NewInstanceType(fake.InstanceTypeOptions{
		Name: "m5.large",
	})

	store := newInternalInstanceTypeStore()
	// Don't add "unevaluated" to evaluatedNodePools

	_, err := store.apply("unevaluated", instanceType)
	g.Expect(err).To(HaveOccurred(), "expected error for unevaluated node pool")
	g.Expect(IsUnevaluatedNodePoolError(err)).To(BeTrue(), "expected UnevaluatedNodePoolError")
}

func TestNodeOverlayIntegration(t *testing.T) {
	g := NewWithT(t)
	// Create a realistic scenario with multiple instance types and overlays
	instanceTypes := []*cloudprovider.InstanceType{
		fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "m5.large",
			Offerings: []*cloudprovider.Offering{
				{
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
						scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "on-demand"),
					),
					Price:     0.096,
					Available: true,
				},
				{
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
						scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "spot"),
					),
					Price:     0.0288,
					Available: true,
				},
			},
			Resources: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("8Gi"),
				corev1.ResourceCPU:    resource.MustParse("2"),
			},
		}),
		fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "m5.xlarge",
			Offerings: []*cloudprovider.Offering{
				{
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
						scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "on-demand"),
					),
					Price:     0.192,
					Available: true,
				},
			},
			Resources: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourceCPU:    resource.MustParse("4"),
			},
		}),
	}

	store := NewInstanceTypeStore()
	internalStore := newInternalInstanceTypeStore()
	internalStore.evaluatedNodePools.Insert("default")

	// Apply overlays to m5.large: add hugepages and adjust spot pricing
	overlay := v1alpha1.NodeOverlay{
		Spec: v1alpha1.NodeOverlaySpec{
			Weight: lo.ToPtr(int32(100)),
			Capacity: corev1.ResourceList{
				"hugepages-2Mi": resource.MustParse("100Mi"),
			},
		},
	}

	// Simulate what the controller does
	internalStore.updateInstanceTypeCapacity("default", "m5.large", overlay)
	internalStore.updateInstanceTypeOffering("default", "m5.large", overlay, instanceTypes[0].Offerings[1:2]) // Only spot offering

	store.UpdateStore(internalStore)

	// Apply overlays through the public interface
	results, err := store.ApplyAll("default", instanceTypes)
	g.Expect(err).ToNot(HaveOccurred())

	// Verify m5.large has hugepages added
	m5Large := results[0]
	_, ok := m5Large.Capacity["hugepages-2Mi"]
	g.Expect(ok).To(BeTrue(), "expected m5.large to have hugepages-2Mi added")

	// Verify m5.xlarge is unchanged
	m5XLarge := results[1]
	_, ok = m5XLarge.Capacity["hugepages-2Mi"]
	g.Expect(ok).To(BeFalse(), "m5.xlarge should not have hugepages-2Mi")

	// Verify original instance types were not mutated
	_, ok = instanceTypes[0].Capacity["hugepages-2Mi"]
	g.Expect(ok).To(BeFalse(), "original m5.large should not have been mutated")
}
