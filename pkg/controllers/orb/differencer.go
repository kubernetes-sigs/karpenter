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
	"bytes"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	v1 "k8s.io/api/core/v1"
)

// Functions to check the differences in all the fields of a SchedulingInput (except the timestamp)
// This function takes an old Scheduling Input and a new one and returns a SchedulingInput of only the differences.
func (si *SchedulingInput) Diff(oldSi *SchedulingInput) (*SchedulingInput, *SchedulingInput, *SchedulingInput) {
	pendingPodsAdded, pendingPodsRemoved, pendingPodsChanged := diffPods(oldSi.PendingPods, si.PendingPods)
	stateNodesAdded, stateNodesRemoved, stateNodesChanged := diffStateNodes(oldSi.StateNodesWithPods, si.StateNodesWithPods)
	instanceTypesAdded, instanceTypesRemoved, instanceTypesChanged := diffInstanceTypes(oldSi.InstanceTypes, si.InstanceTypes)

	diffAdded := &SchedulingInput{
		Timestamp:          si.Timestamp, // i.e. the time of those (newer) differences
		PendingPods:        pendingPodsAdded,
		StateNodesWithPods: stateNodesAdded,
		InstanceTypes:      instanceTypesAdded,
	}

	diffRemoved := &SchedulingInput{
		Timestamp:          si.Timestamp,
		PendingPods:        pendingPodsRemoved,
		StateNodesWithPods: stateNodesRemoved,
		InstanceTypes:      instanceTypesRemoved,
	}

	diffChanged := &SchedulingInput{
		Timestamp:          si.Timestamp,
		PendingPods:        pendingPodsChanged,
		StateNodesWithPods: stateNodesChanged,
		InstanceTypes:      instanceTypesChanged,
	}

	if len(pendingPodsAdded)+len(stateNodesAdded)+len(instanceTypesAdded) == 0 {
		diffAdded = &SchedulingInput{}
	} else {
		fmt.Println("Diff Scheduling Input added is... ", diffAdded.String()) // Test print, delete later
	}

	if len(pendingPodsRemoved)+len(stateNodesRemoved)+len(instanceTypesRemoved) == 0 {
		diffRemoved = &SchedulingInput{}
	} else {
		fmt.Println("Diff Scheduling Input removed is... ", diffRemoved.String()) // Test print, delete later
	}

	if len(pendingPodsChanged)+len(stateNodesChanged)+len(instanceTypesChanged) == 0 {
		diffChanged = &SchedulingInput{}
	} else {
		fmt.Println("Diff Scheduling Input changed is... ", diffChanged.String()) // Test print, delete later
	}

	return diffAdded, diffRemoved, diffChanged
}

// This is the diffPods function which gets the differences between pods
func diffPods(oldPods, newPods []*v1.Pod) ([]*v1.Pod, []*v1.Pod, []*v1.Pod) {
	// Convert the slices to sets for efficient difference calculation
	oldPodSet := map[string]*v1.Pod{}
	for _, pod := range oldPods {
		oldPodSet[pod.GetName()] = pod
	}

	newPodSet := map[string]*v1.Pod{}
	for _, pod := range newPods {
		newPodSet[pod.GetName()] = pod
	}

	// Find the differences between the sets
	added := []*v1.Pod{}
	removed := []*v1.Pod{}
	changed := []*v1.Pod{}
	for _, newPod := range newPods {
		oldPod, exists := oldPodSet[newPod.GetName()]

		// If pod is new, add to "added"
		if !exists {
			added = append(added, newPod)
			continue
		}

		// If pod has changed, add the whole changed pod
		// Simplification / Opportunity to optimize -- Only add sub-field.
		//    This requires more book-keeping on object reconstruction from logs later on.
		if hasPodChanged(oldPod, newPod) {
			changed = append(changed, newPod)
		}
	}

	// Get the remainder "removed" pods
	for _, oldPod := range oldPods {
		if _, exists := newPodSet[oldPod.GetName()]; !exists {
			removed = append(removed, oldPod)
		}
	}

	return added, removed, changed
}

// This is the diffStateNodes function which gets the differences between statenodes
func diffStateNodes(oldStateNodesWithPods, newStateNodesWithPods []*StateNodeWithPods) ([]*StateNodeWithPods, []*StateNodeWithPods, []*StateNodeWithPods) {
	// Convert the slices to sets for efficient difference calculation
	oldStateNodeSet := make(map[string]*StateNodeWithPods, len(oldStateNodesWithPods))
	for _, stateNodeWithPods := range oldStateNodesWithPods {
		oldStateNodeSet[stateNodeWithPods.GetName()] = stateNodeWithPods
	}

	newStateNodeSet := make(map[string]*StateNodeWithPods, len(newStateNodesWithPods))
	for _, stateNodeWithPods := range newStateNodesWithPods {
		newStateNodeSet[stateNodeWithPods.GetName()] = stateNodeWithPods
	}

	// Find the differences between the sets
	added := []*StateNodeWithPods{}
	removed := []*StateNodeWithPods{}
	changed := []*StateNodeWithPods{}
	for _, newStateNodeWithPods := range newStateNodesWithPods {
		oldStateNodeWithPods, exists := oldStateNodeSet[newStateNodeWithPods.GetName()]

		// If stateNode is new, add to "added"
		if !exists {
			added = append(added, newStateNodeWithPods)
			continue
		}

		// If stateNode has changed, add the whole changed stateNodeWithPods
		if hasStateNodeWithPodsChanged(oldStateNodeWithPods, newStateNodeWithPods) {
			changed = append(changed, newStateNodeWithPods)
		}
	}

	// Get the remainder "removed" stateNodesWithPods
	for _, oldStateNodeWithPods := range oldStateNodesWithPods {
		if _, exists := newStateNodeSet[oldStateNodeWithPods.GetName()]; !exists {
			removed = append(removed, oldStateNodeWithPods)
		}
	}

	return added, removed, changed
}

// This is the diffInstanceTypes function which gets the differences between instance types
func diffInstanceTypes(oldTypes, newTypes []*cloudprovider.InstanceType) ([]*cloudprovider.InstanceType,
	[]*cloudprovider.InstanceType, []*cloudprovider.InstanceType) {

	// Convert the slices to sets for efficient difference calculation
	oldTypeSet := map[string]*cloudprovider.InstanceType{}
	for _, instanceType := range oldTypes {
		oldTypeSet[instanceType.Name] = instanceType
	}

	newTypeSet := map[string]*cloudprovider.InstanceType{}
	for _, instanceType := range newTypes {
		newTypeSet[instanceType.Name] = instanceType
	}

	// Find the differences between the sets
	added := []*cloudprovider.InstanceType{}
	removed := []*cloudprovider.InstanceType{}
	changed := []*cloudprovider.InstanceType{}
	for _, newType := range newTypes {
		oldType, exists := oldTypeSet[newType.Name]

		// If instanceType is new, add to "added"
		if !exists {
			added = append(added, newType)
			continue
		}

		// If instanceType has changed, add the whole changed resource
		if hasInstanceTypeChanged(oldType, newType) {
			changed = append(changed, newType)
		}
	}

	// Get the remainder (removed) types
	for _, oldType := range oldTypes {
		if _, exists := newTypeSet[oldType.Name]; !exists {
			removed = append(removed, oldType)
		}
	}

	return added, removed, changed
}

// TODO: change these to checking only reduced-fields, so that DeepEqual isn't required.
// Maybe just proto it and check proto.Equal because that already reduces it to those fields.
func hasPodChanged(oldPod, newPod *v1.Pod) bool {
	return !equality.Semantic.DeepEqual(oldPod, newPod)
}

func hasStateNodeWithPodsChanged(oldStateNodeWithPods, newStateNodeWithPods *StateNodeWithPods) bool {
	return !equality.Semantic.DeepEqual(oldStateNodeWithPods, newStateNodeWithPods)
}

// Checking equality on only fields I've reduced it to (i.e. Name Requirements Offerings)
func hasInstanceTypeChanged(oldInstanceType, newInstanceType *cloudprovider.InstanceType) bool {
	return !equality.Semantic.DeepEqual(oldInstanceType.Name, newInstanceType.Name) ||
		!structEqual(oldInstanceType.Offerings, newInstanceType.Offerings) ||
		!structEqual(oldInstanceType.Requirements, newInstanceType.Requirements)
}

// // Equality test for requirements based on the three keys I'm tracking for them, namely
// // karpenter.sh/capacity-type, topology.k8s.aws/zone-id, topology.kubernetes.io/zone and their values
// func requirementsEqual(oldrequirements scheduling.Requirements, newrequirements scheduling.Requirements) bool {
// 	return oldrequirements.Get("karpenter.sh/capacity-type") != newrequirements.Get("karpenter.sh/capacity-type") ||
// 		oldrequirements.Get("topology.k8s.aws/zone-id") != newrequirements.Get("topology.k8s.aws/zone-id") ||
// 		oldrequirements.Get("topology.kubernetes.io/zone") != newrequirements.Get("topology.kubernetes.io/zone")
// }

// TODO: Likely inefficient equality checking for nested types Offerings and Requirements,
// but both have unexported types not compatible with DeepEqual
func structEqual(a, b interface{}) bool {
	aBytes, _ := json.Marshal(a)
	bBytes, _ := json.Marshal(b)
	return bytes.Equal(aBytes, bBytes)
}
