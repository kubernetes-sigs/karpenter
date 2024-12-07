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
	"container/heap"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestNodeClaimHeap_PopOrder(t *testing.T) {
	// Create NodeClaims with different pod counts
	nc1 := &NodeClaim{Pods: []*corev1.Pod{{}, {}}}     // 2 pods
	nc2 := &NodeClaim{Pods: []*corev1.Pod{{}}}         // 1 pod
	nc3 := &NodeClaim{Pods: []*corev1.Pod{{}, {}, {}}} // 3 pods
	nc4 := &NodeClaim{Pods: []*corev1.Pod{}}           // 0 pods

	// Initialize heap with NodeClaims
	h := NewNodeClaimHeap([]*NodeClaim{nc1, nc2, nc3, nc4})
	heap.Init(h)

	// Pop items and verify they come out in ascending order of pod count
	expected := []*NodeClaim{nc4, nc2, nc1, nc3}

	for i := 0; i < len(expected); i++ {
		item := heap.Pop(h).(*NodeClaim)
		assert.Equal(t, len(expected[i].Pods), len(item.Pods),
			"Expected NodeClaim with %d pods, got %d pods",
			len(expected[i].Pods), len(item.Pods))
	}

	// Verify heap is empty
	assert.Equal(t, 0, h.Len(), "Heap should be empty after popping all items")
}
