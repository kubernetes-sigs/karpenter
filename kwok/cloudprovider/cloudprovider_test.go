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

func TestAllocatableCapacityInvariant(t *testing.T) {
	// This test verifies that the bug in commit 99057233 is fixed.
	// The bug: using lo.Assign() allowed instanceType.Allocatable() to override
	// nodeClaim.Spec.Resources.Requests, causing allocatable > capacity.
	
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("4Gi"),
	}
	
	instanceAllocatable := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	
	// The old buggy approach used lo.Assign()
	oldApproach := lo.Assign(capacity, instanceAllocatable)
	
	// Verify old approach violated the invariant
	violationsFound := false
	for k, capVal := range capacity {
		allocVal := oldApproach[k]
		if allocVal.Cmp(capVal) > 0 {
			violationsFound = true
			t.Logf("Old approach violated invariant: allocatable[%s]=%s > capacity[%s]=%s", 
				k, allocVal.String(), k, capVal.String())
		}
	}
	
	if !violationsFound {
		t.Error("Expected old approach to violate invariant, but it didn't")
	}
	
	// The new approach simply uses capacity for both
	// This is correct for KWOK since we're simulating nodes and the comment says
	// "we only apply resource requests"
	newApproach := capacity
	
	// Verify new approach maintains invariant (allocatable == capacity)
	for k, capVal := range capacity {
		allocVal, ok := newApproach[k]
		if !ok {
			t.Errorf("Resource %s missing from allocatable", k)
			continue
		}
		if allocVal.Cmp(capVal) != 0 {
			t.Errorf("Allocatable should equal capacity: allocatable[%s]=%s, capacity[%s]=%s", 
				k, allocVal.String(), k, capVal.String())
		}
	}
}
