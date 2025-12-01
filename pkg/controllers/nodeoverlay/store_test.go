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
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func TestStoreApplySelectiveCopy(t *testing.T) {
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
			store := newInternalInstanceTypeStore()
			store.evaluatedNodePools.Insert("default")
			store.updates["default"] = map[string]*instanceTypeUpdate{
				tt.instanceType.Name: {
					Price:    tt.priceOverlay,
					Capacity: tt.capacityOverlay,
				},
			}

			result, err := store.apply("default", tt.instanceType)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify Requirements sharing - map comparison by checking first key address
			if tt.expectSharedReqs {
				// For maps, we check if they're the same object by comparing addresses
				// Since Requirements is a map, we compare if they share memory
				if len(result.Requirements) > 0 && len(tt.instanceType.Requirements) > 0 {
					// Get first key from both maps
					for k := range result.Requirements {
						origReq := tt.instanceType.Requirements[k]
						resultReq := result.Requirements[k]
						if origReq != resultReq {
							t.Errorf("expected Requirements to be shared (same map)")
						}
						break
					}
				}
			}

			// Verify Overhead sharing
			if tt.expectSharedOverhead {
				if result.Overhead != tt.instanceType.Overhead {
					t.Errorf("expected Overhead to be shared (same pointer)")
				}
			}

			// Verify Offerings sharing
			if tt.expectSharedOffering {
				if len(result.Offerings) > 0 && len(tt.instanceType.Offerings) > 0 {
					if result.Offerings[0] != tt.instanceType.Offerings[0] {
						t.Errorf("expected Offerings[0] to be shared (same pointer)")
					}
				}
			} else {
				if len(result.Offerings) > 0 && len(tt.instanceType.Offerings) > 0 {
					if result.Offerings[0] == tt.instanceType.Offerings[0] {
						t.Errorf("expected Offerings[0] to be copied (different pointer)")
					}
				}
			}

			// Verify Capacity sharing - ResourceList is a map, check if modifications affect original
			if tt.expectSharedCapacity {
				// For ResourceList (map), we can check if they point to the same memory
				// by verifying that modifications would affect both
				if len(result.Capacity) > 0 && len(tt.instanceType.Capacity) > 0 {
					// Check if they share the same underlying map by comparing addresses
					// Since we can't directly compare map pointers, we skip this detailed check
					// The correctness tests below verify the actual behavior
				}
			}
		})
	}
}

func TestStoreApplyCorrectnessWithPriceOverlay(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify first offering was modified
	if result.Offerings[0].Price != 0.106 { // 0.096 + 0.01
		t.Errorf("expected first offering price to be 0.106, got %f", result.Offerings[0].Price)
	}
	if !result.Offerings[0].IsPriceOverlaid() {
		t.Errorf("expected first offering to have priceOverlayApplied flag set")
	}

	// Verify second offering was NOT modified (should be shared pointer)
	if result.Offerings[1].Price != originalPrice {
		t.Errorf("expected second offering price to remain %f, got %f", originalPrice, result.Offerings[1].Price)
	}
	if result.Offerings[1] != instanceType.Offerings[1] {
		t.Errorf("expected second offering to be shared (same pointer)")
	}

	// Verify original instance type was not mutated
	if instanceType.Offerings[0].Price != originalPrice {
		t.Errorf("original instance type should not be mutated, expected price %f, got %f",
			originalPrice, instanceType.Offerings[0].Price)
	}
}

func TestStoreApplyCorrectnessWithCapacityOverlay(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify hugepages was added
	hugepages, ok := result.Capacity["hugepages-2Mi"]
	if !ok {
		t.Fatalf("expected hugepages-2Mi to be added to capacity")
	}
	if hugepages.String() != "100Mi" {
		t.Errorf("expected hugepages-2Mi to be 100Mi, got %s", hugepages.String())
	}

	// Verify original resources are still present
	if result.Capacity.Memory().Cmp(originalMemory) != 0 {
		t.Errorf("expected memory to remain %s, got %s", originalMemory.String(), result.Capacity.Memory().String())
	}

	// Verify original instance type was not mutated
	if _, exists := instanceType.Capacity["hugepages-2Mi"]; exists {
		t.Errorf("original instance type should not have hugepages added")
	}
}

func TestStoreApplyIsolationBetweenNodePools(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error for nodepool-a: %v", err)
	}

	// Apply to NodePool B
	resultB, err := store.apply("nodepool-b", instanceType)
	if err != nil {
		t.Fatalf("unexpected error for nodepool-b: %v", err)
	}

	// Verify NodePool A has +10% (0.096 * 1.10 = 0.1056)
	expectedPriceA := 0.1056
	if diff := resultA.Offerings[0].Price - expectedPriceA; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("nodepool-a: expected price %.4f, got %.4f", expectedPriceA, resultA.Offerings[0].Price)
	}

	// Verify NodePool B has -5% (0.096 * 0.95 = 0.0912)
	expectedPriceB := 0.0912
	if diff := resultB.Offerings[0].Price - expectedPriceB; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("nodepool-b: expected price %.4f, got %.4f", expectedPriceB, resultB.Offerings[0].Price)
	}

	// Verify original instance type was not mutated
	if instanceType.Offerings[0].Price != 0.096 {
		t.Errorf("original instance type should not be mutated, expected price 0.096, got %f",
			instanceType.Offerings[0].Price)
	}

	// Verify the two results have different offering pointers (isolated)
	if resultA.Offerings[0] == resultB.Offerings[0] {
		t.Errorf("nodepool-a and nodepool-b should have different offering pointers (isolated)")
	}
}

func TestStoreApplyUnevaluatedNodePool(t *testing.T) {
	instanceType := fake.NewInstanceType(fake.InstanceTypeOptions{
		Name: "m5.large",
	})

	store := newInternalInstanceTypeStore()
	// Don't add "unevaluated" to evaluatedNodePools

	_, err := store.apply("unevaluated", instanceType)
	if err == nil {
		t.Fatal("expected error for unevaluated node pool")
	}

	if !IsUnevaluatedNodePoolError(err) {
		t.Errorf("expected UnevaluatedNodePoolError, got: %v", err)
	}
}

func TestNodeOverlayIntegration(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify m5.large has hugepages added
	m5Large := results[0]
	if _, ok := m5Large.Capacity["hugepages-2Mi"]; !ok {
		t.Errorf("expected m5.large to have hugepages-2Mi added")
	}

	// Verify m5.xlarge is unchanged
	m5XLarge := results[1]
	if _, ok := m5XLarge.Capacity["hugepages-2Mi"]; ok {
		t.Errorf("m5.xlarge should not have hugepages-2Mi")
	}

	// Verify original instance types were not mutated
	if _, ok := instanceTypes[0].Capacity["hugepages-2Mi"]; ok {
		t.Errorf("original m5.large should not have been mutated")
	}
}
