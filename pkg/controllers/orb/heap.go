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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1api "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pb "sigs.k8s.io/karpenter/pkg/controllers/orb/proto"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

type TimestampedType interface {
	GetTime() time.Time
}

// MinTimeHeap is a generic min-heap implementation with the Timestamp field defined as the comparator
type MinTimeHeap[T TimestampedType] []T

type SchedulingInputHeap = MinTimeHeap[SchedulingInput]
type SchedulingMetadataHeap = MinTimeHeap[SchedulingMetadata]

func (h MinTimeHeap[T]) Len() int {
	return len(h)
}

// Compares timestamps for a min heap. Oldest elements pop first.
func (h MinTimeHeap[T]) Less(i, j int) bool {
	return h[i].GetTime().Before(h[j].GetTime())
}

func (h MinTimeHeap[T]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *MinTimeHeap[T]) Push(x interface{}) {
	*h = append(*h, x.(T))
}

func (h *MinTimeHeap[T]) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func NewMinHeap[T TimestampedType]() *MinTimeHeap[T] {
	h := &MinTimeHeap[T]{}
	heap.Init(h)
	return h
}

// Pushes Provisioner Scheduler inputs to their heap for the ORB controller to reconcile.
func (h *MinTimeHeap[SchedulingInput]) LogSchedulingInput(ctx context.Context, kubeClient client.Client, scheduledTime time.Time, pods []*v1.Pod, stateNodes []*state.StateNode,
	bindings map[types.NamespacedName]string, instanceTypes map[string][]*cloudprovider.InstanceType, topology *scheduler.Topology, daemonSetPods []*v1.Pod) {
	si := NewSchedulingInput(ctx, kubeClient, scheduledTime, pods, stateNodes, bindings, instanceTypes, topology, daemonSetPods)
	heap.Push(h, si)
}

// Pushes scheduling action metadata being scheduled by the Provisioner to their heap for the ORB controller to reconcile.
// Re-setting scheduling time here resolves the potential time difference between the start of an action call (consolidation/drift)
// and its subsequent provisioning scheduling. This allows us to associate metadata with its respective scheduling action.
func (h *MinTimeHeap[SchedulingMetadata]) LogSchedulingAction(ctx context.Context, schedulingAction string, schedulingTime time.Time) {
	if schedulingAction == "" { // If scheduling reason is not set already (i.e. scheduling was not prompted by a Consolidation or Drift) it is a normal provisioning action
		schedulingAction = v1api.ProvisioningSchedulingAction
	}

	heap.Push(h, NewSchedulingMetadata(schedulingAction, schedulingTime))
}

// Converts from scheduling metadata's Karpenter representation to protobuf. It is nearly-symmetric to its Reconstruct function.
func protoSchedulingMetadataMap(heap *SchedulingMetadataHeap) *pb.SchedulingMetadataMap {
	mapping := &pb.SchedulingMetadataMap{}
	for heap.Len() > 0 {
		metadata := heap.Pop().(SchedulingMetadata)
		entry := protoSchedulingMetadata(metadata)
		mapping.Entries = append(mapping.Entries, entry)
	}
	return mapping
}

// Reconstructs scheduling metadata as a slice instead of back as a heap since each file will be heapified in aggregate.
// It is nearly-symmetric to its proto function above; the slice of metadata is more meaningful than the original map as a reconstruction.
func ReconstructAllSchedulingMetadata(mapping *pb.SchedulingMetadataMap) []*SchedulingMetadata {
	metadata := []*SchedulingMetadata{}
	for _, entry := range mapping.Entries {
		metadatum := reconstructSchedulingMetadata(entry)
		metadata = append(metadata, &metadatum)
	}
	return metadata
}
