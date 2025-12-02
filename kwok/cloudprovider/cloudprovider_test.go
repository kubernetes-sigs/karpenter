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
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMinResourceList(t *testing.T) {
	tests := []struct {
		name     string
		a        corev1.ResourceList
		b        corev1.ResourceList
		expected corev1.ResourceList
	}{
		{
			name: "allocatable smaller than capacity",
			a: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			b: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
		{
			name: "allocatable larger than capacity - should cap at capacity",
			a: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			b: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
		{
			name: "mixed values",
			a: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			b: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
		{
			name: "resources only in a (capacity)",
			a: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
			b: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		},
		{
			name: "resources only in b (allocatable) - should NOT be included",
			a: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			b: corev1.ResourceList{
				corev1.ResourceCPU:                resource.MustParse("2"),
				corev1.ResourceMemory:             resource.MustParse("4Gi"),
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				// nvidia.com/gpu should NOT be included since it's not in capacity
			},
		},
		{
			name: "hugepages in allocatable but not capacity",
			a: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			b: corev1.ResourceList{
				corev1.ResourceCPU:                           resource.MustParse("3900m"),
				corev1.ResourceMemory:                        resource.MustParse("7Gi"),
				corev1.ResourceName("hugepages-2Mi"): resource.MustParse("1Gi"),
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3900m"),
				corev1.ResourceMemory: resource.MustParse("7Gi"),
				// hugepages should NOT be included since it's not in capacity
			},
		},
		{
			name: "empty capacity",
			a:    corev1.ResourceList{},
			b: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			expected: corev1.ResourceList{},
		},
		{
			name: "empty allocatable",
			a: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			b: corev1.ResourceList{},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
		{
			name: "zero values",
			a: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("0"),
				corev1.ResourceMemory: resource.MustParse("0"),
			},
			b: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("0"),
				corev1.ResourceMemory: resource.MustParse("0"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := minResourceList(tt.a, tt.b)
			
			// Check all expected resources are present with correct values
			for k, expectedVal := range tt.expected {
				resultVal, ok := result[k]
				if !ok {
					t.Errorf("expected resource %s not found in result", k)
					continue
				}
				if resultVal.Cmp(expectedVal) != 0 {
					t.Errorf("resource %s: expected %s, got %s", k, expectedVal.String(), resultVal.String())
				}
			}
			
			// Check no unexpected resources
			if len(result) != len(tt.expected) {
				t.Errorf("expected %d resources, got %d", len(tt.expected), len(result))
				for k := range result {
					if _, ok := tt.expected[k]; !ok {
						t.Errorf("unexpected resource in result: %s", k)
					}
				}
			}
		})
	}
}

func TestAllocatableCapacityInvariant(t *testing.T) {
	// This test verifies that allocatable is always <= capacity
	// Simulating the scenario where instanceType.Allocatable() returns larger values than requests
	
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("4Gi"),
	}
	
	instanceAllocatable := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	
	// Using the old approach (lo.Assign) would violate the invariant
	oldApproach := lo.Assign(capacity, instanceAllocatable)
	
	// Verify old approach violates invariant
	violationsFound := false
	for k, capVal := range capacity {
		allocVal := oldApproach[k]
		if allocVal.Cmp(capVal) > 0 {
			violationsFound = true
			t.Logf("Old approach violates invariant: allocatable[%s]=%s > capacity[%s]=%s", 
				k, allocVal.String(), k, capVal.String())
		}
	}
	
	if !violationsFound {
		t.Error("Expected old approach to violate invariant, but it didn't")
	}
	
	// Using the new approach (minResourceList) should maintain the invariant
	newApproach := minResourceList(capacity, instanceAllocatable)
	
	// Verify new approach maintains invariant for all resources in capacity
	for k, capVal := range capacity {
		allocVal, ok := newApproach[k]
		if !ok {
			t.Errorf("Resource %s missing from allocatable", k)
			continue
		}
		if allocVal.Cmp(capVal) > 0 {
			t.Errorf("New approach violates invariant: allocatable[%s]=%s > capacity[%s]=%s", 
				k, allocVal.String(), k, capVal.String())
		}
	}
}

func TestMinResourceListOnlyIncludesCapacityResources(t *testing.T) {
	// Test that only resources in capacity are included in result
	// This is correct behavior for KWOK since capacity = nodeClaim.Spec.Resources.Requests
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	
	allocatable := corev1.ResourceList{
		corev1.ResourceCPU:                     resource.MustParse("3900m"),
		corev1.ResourceMemory:                  resource.MustParse("7Gi"),
		corev1.ResourceName("nvidia.com/gpu"):  resource.MustParse("2"),
		corev1.ResourceName("hugepages-2Mi"):   resource.MustParse("1Gi"),
		corev1.ResourceName("example.com/foo"): resource.MustParse("10"),
	}
	
	result := minResourceList(capacity, allocatable)
	
	// Verify extended resources are NOT present (since they're not in capacity)
	extendedResources := []corev1.ResourceName{
		"nvidia.com/gpu",
		"hugepages-2Mi",
		"example.com/foo",
	}
	
	for _, res := range extendedResources {
		if _, ok := result[res]; ok {
			t.Errorf("Extended resource %s should not be in result (not in capacity)", res)
		}
	}
	
	// Verify standard resources respect capacity limits
	cpuResult := result[corev1.ResourceCPU]
	cpuCapacity := capacity[corev1.ResourceCPU]
	if cpuResult.Cmp(cpuCapacity) > 0 {
		t.Errorf("CPU allocatable exceeds capacity")
	}
	memResult := result[corev1.ResourceMemory]
	memCapacity := capacity[corev1.ResourceMemory]
	if memResult.Cmp(memCapacity) > 0 {
		t.Errorf("Memory allocatable exceeds capacity")
	}
	
	// Verify only capacity resources are in result
	if len(result) != len(capacity) {
		t.Errorf("Result should only contain resources from capacity. Expected %d, got %d", len(capacity), len(result))
	}
}
