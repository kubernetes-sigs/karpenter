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
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// This defines a min-heap of SchedulingInputs by slice,
// with the Timestamp field defined as the comparator
type SchedulingInputHeap []SchedulingInput //heaps are thread-safe in container/heap

func (h SchedulingInputHeap) Len() int {
	return len(h)
}

// This compares timestamps for a min heap, so that older inputs pop first.
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
// TODO: add all inputs I want to log in New constructor
func (h *SchedulingInputHeap) LogProvisioningScheduler(ctx context.Context, kubeClient client.Client, scheduledTime time.Time,
	pods []*v1.Pod, stateNodes []*state.StateNode, instanceTypes map[string][]*cloudprovider.InstanceType) {
	si := NewSchedulingInput(ctx, kubeClient, scheduledTime, pods, stateNodes, instanceTypes["default"])
	si = si.Reduce() // Strip out "unnecessary" parts of the data structure...
	h.Push(si)       // sends that scheduling input into the data structure to be popped in batch to go to PV as a protobuf
}

type SchedulingMetadataHeap []SchedulingMetadata

func (h SchedulingMetadataHeap) Len() int {
	return len(h)
}

func (h SchedulingMetadataHeap) Less(i, j int) bool {
	return h[i].Timestamp.Before(h[j].Timestamp)
}

func (h SchedulingMetadataHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *SchedulingMetadataHeap) Push(x interface{}) {
	*h = append(*h, x.(SchedulingMetadata))
}

func (h *SchedulingMetadataHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func NewSchedulingMetadataHeap() *SchedulingMetadataHeap {
	h := &SchedulingMetadataHeap{}
	heap.Init(h)
	return h
}

// This function will log scheduling action to PV
func (h *SchedulingMetadataHeap) LogSchedulingAction(ctx context.Context, schedulingTime time.Time) error {
	metadata, ok := GetSchedulingMetadata(ctx)

	// The resolves the time difference between the start of a consolidation call and the subsequent provisioning scheduling
	metadata.Timestamp = schedulingTime

	if !ok { // Provisioning metadata is not set, set it to the default - normal provisioning action
		ctx = WithSchedulingMetadata(ctx, "normal-provisioning", schedulingTime)
		metadata, _ = GetSchedulingMetadata(ctx) // Get it again to update metadata
	}
	h.Push(metadata)
	return nil
}
