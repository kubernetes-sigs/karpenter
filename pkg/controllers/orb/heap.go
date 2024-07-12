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

package orb

import (
	"container/heap"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// This defines a min-heap of SchedulingInputs by slice,
// with the Timestamp field defined as the comparator
type SchedulingInputHeap []SchedulingInput //heaps are thread-safe in container/heap

func (h SchedulingInputHeap) Len() int {
	return len(h)
}

func (h SchedulingInputHeap) Less(i, j int) bool {
	return h[i].Timestamp.Before(h[j].Timestamp)
}

func (h SchedulingInputHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *SchedulingInputHeap) Push(x interface{}) {
	*h = append(*h, x.(SchedulingInput))
}

func (h *SchedulingInputHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func NewSchedulingInputHeap() *SchedulingInputHeap {
	h := &SchedulingInputHeap{}
	heap.Init(h)
	return h
}

// Function for logging everything in the Provisioner Scheduler (i.e. pending pods, statenodes...)
func (h *SchedulingInputHeap) LogProvisioningScheduler(pods []*v1.Pod, stateNodes []*state.StateNode, instanceTypes map[string][]*cloudprovider.InstanceType) {
	si := NewSchedulingInput(pods, stateNodes, instanceTypes["default"]) // TODO: add all inputs I want to log
	h.Push(si)                                                           // sends that scheduling input into the data structure to be popped in batch to go to PV as a protobuf
}
