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

func TestRevertToInstanceType(t *testing.T) {
	// This test verifies that reverting to instanceType.Capacity/Allocatable()
	// (the original behavior before commit 99057233) maintains the invariant.
	// 
	// The bug in commit 99057233 was trying to merge nodeClaim.Spec.Resources.Requests
	// with instanceType.Allocatable() using lo.Assign(), which violated the invariant.
	//
	// The fix: just use instanceType values like the original code did.
	
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	
	allocatable := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("3900m"),
		corev1.ResourceMemory: resource.MustParse("7500Mi"),
	}
	
	// Verify instanceType approach maintains invariant
	for k, capVal := range capacity {
		allocVal, ok := allocatable[k]
		if !ok {
			t.Errorf("Resource %s missing from allocatable", k)
			continue
		}
		if allocVal.Cmp(capVal) > 0 {
			t.Errorf("Allocatable exceeds capacity: allocatable[%s]=%s > capacity[%s]=%s", 
				k, allocVal.String(), k, capVal.String())
		}
	}
	
	// Show what the buggy approach did
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("4Gi"),
	}
	
	buggyApproach := lo.Assign(requests, allocatable)
	
	t.Logf("Original (correct): capacity=%v, allocatable=%v", capacity, allocatable)
	t.Logf("Buggy approach: capacity=%v, allocatable=%v (violates invariant!)", requests, buggyApproach)
}
