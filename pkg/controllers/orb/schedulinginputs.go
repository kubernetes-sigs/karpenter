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
	"fmt"
	"time"

	"github.com/samber/lo"
	//"google.golang.org/protobuf/proto"
	proto "github.com/gogo/protobuf/proto"
	v1 "k8s.io/api/core/v1"
)

// Timestamp, dynamic inputs (like pending pods, statenodes, etc.)
type SchedulingInput struct {
	Timestamp   time.Time
	PendingPods []*v1.Pod
	//all the other scheduling inputs...
}

func (si SchedulingInput) String() string {
	return fmt.Sprintf("Timestamp: %v\nPendingPods:\n%v",
		si.Timestamp.Format("2006-01-02_15-04-05"),
		PodsToString(si.PendingPods))
}

// Function takes a slice of pod pointers and returns a string representation of the pods

// Function take a Scheduling Input to []byte, marshalled as a protobuf
// TODO: With a custom-defined .proto, this will look different.
func (si SchedulingInput) Marshal() ([]byte, error) {
	podList := &v1.PodList{
		Items: make([]v1.Pod, 0, len(si.PendingPods)),
	}

	for _, podPtr := range si.PendingPods {
		podList.Items = append(podList.Items, *podPtr)
	}
	return podList.Marshal()

	// // Create a slice to store the wire format data
	// podDataSlice := make([][]byte, 0, len(si.PendingPods))

	// // Iterate over the slice of Pods and marshal each one to its wire format
	// for _, pod := range si.PendingPods {
	// 	podData, err := proto.Marshal(pod)
	// 	if err != nil {
	// 		fmt.Println("Error marshaling pod:", err)
	// 		continue
	// 	}
	// 	podDataSlice = append(podDataSlice, podData)
	// }

	// // Create an ORBLogEntry message
	// entry := &ORBLogEntry{
	// 	Timestamp:      si.Timestamp.Format("2006-01-02_15-04-05"),
	// 	PendingpodData: podDataSlice,
	// }

	// return proto.Marshal(entry)
}

// func UnmarshalSchedulingInput(data []byte) (*SchedulingInput, error) {
// 	// Unmarshal the data into an ORBLogEntry struct
// 	entry := &ORBLogEntry{}
// 	if err := proto.Unmarshal(data, entry); err != nil {
// 		return nil, fmt.Errorf("failed to unmarshal ORBLogEntry: %v", err)
// 	}

// 	// Parse the timestamp
// 	timestamp, err := time.Parse("2006-01-02_15-04-05", entry.Timestamp)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to parse timestamp: %v", err)
// 	}

// 	// Unmarshal the PendingpodData into v1.Pod objects
// 	pendingPods := make([]*v1.Pod, 0, len(entry.PendingpodData))
// 	for _, podData := range entry.PendingpodData {
// 		var pod v1.Pod
// 		if err := proto.Unmarshal(podData, &pod); err != nil {
// 			return nil, fmt.Errorf("failed to unmarshal pod: %v", err)
// 		}
// 		pendingPods = append(pendingPods, &pod)
// 	}

// 	// Create a new SchedulingInput struct
// 	schedulingInput := &SchedulingInput{
// 		Timestamp:   timestamp,
// 		PendingPods: pendingPods,
// 	}

// 	return schedulingInput, nil
// }

// Function to do the reverse, take a scheduling input's []byte and unmarshal it back into a SchedulingInput
func PBToSchedulingInput(timestamp time.Time, data []byte) (SchedulingInput, error) {
	podList := &v1.PodList{}
	if err := proto.Unmarshal(data, podList); err != nil {
		return SchedulingInput{}, fmt.Errorf("unmarshaling pod list, %w", err)
	}
	pods := lo.ToSlicePtr(podList.Items)
	return ReconstructedSchedulingInput(timestamp, pods), nil
}

func NewSchedulingInput(pendingPods []*v1.Pod) SchedulingInput {
	return SchedulingInput{
		Timestamp:   time.Now(),
		PendingPods: pendingPods,
	}
}

// Reconstruct a scheduling input (presumably from a file)
func ReconstructedSchedulingInput(timestamp time.Time, pendingPods []*v1.Pod) SchedulingInput {
	return SchedulingInput{
		Timestamp:   timestamp,
		PendingPods: pendingPods,
	}
}
