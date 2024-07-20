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

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pb "sigs.k8s.io/karpenter/pkg/controllers/orb/proto"

	v1 "k8s.io/api/core/v1"
)

type SchedulingInputDifferences struct {
	Added, Removed, Changed *SchedulingInput
}

type PodDifferences struct {
	Added, Removed, Changed []*v1.Pod
}

type SNPDifferences struct {
	Added, Removed, Changed []*StateNodeWithPods
}

type BindingDifferences struct {
	Added, Removed, Changed map[types.NamespacedName]string
}

type InstanceTypeDifferences struct {
	Added, Removed, Changed []*cloudprovider.InstanceType
}

func MarshalDifferences(differences *SchedulingInputDifferences) ([]byte, error) {
	return proto.Marshal(&pb.Differences{
		Added:   protoSchedulingInput(differences.Added),
		Removed: protoSchedulingInput(differences.Removed),
		Changed: protoSchedulingInput(differences.Changed),
	})
}

func UnmarshalDifferences(differencesData []byte) (*SchedulingInputDifferences, error) {
	differences := &pb.Differences{}

	if err := proto.Unmarshal(differencesData, differences); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Differences: %v", err)
	}

	added, err := reconstructSchedulingInput(differences.GetAdded())
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct Added: %v", err)
	}
	removed, err := reconstructSchedulingInput(differences.GetRemoved())
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct Removed: %v", err)
	}
	changed, err := reconstructSchedulingInput(differences.GetChanged())
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct Changed: %v", err)
	}
	return &SchedulingInputDifferences{
		Added:   added,
		Removed: removed,
		Changed: changed,
	}, nil
}

// Functions to check the differences in all the fields of a SchedulingInput (except the timestamp)
// TODO: Emptiness checking could be improved, so a lone Timestamp doesn't get logged.
func (si *SchedulingInput) Diff(oldSi *SchedulingInput) *SchedulingInputDifferences {
	// Determine the differences in each of the fields of ScheduleInput
	podDiff := diffPods(oldSi.PendingPods, si.PendingPods)
	snpDiff := diffStateNodes(oldSi.StateNodesWithPods, si.StateNodesWithPods)
	bindingsDiff := diffBindings(oldSi.Bindings, si.Bindings)
	itDiff := diffInstanceTypes(oldSi.InstanceTypes, si.InstanceTypes)

	diffAdded := &SchedulingInput{}
	diffRemoved := &SchedulingInput{}
	diffChanged := &SchedulingInput{}

	// If there are added differences, include them
	if len(podDiff.Added) > 0 || len(snpDiff.Added) > 0 || len(bindingsDiff.Added) > 0 || len(itDiff.Added) > 0 {
		diffAdded = NewSchedulingInputReconstruct(si.Timestamp, podDiff.Added, snpDiff.Added, bindingsDiff.Added, itDiff.Added)
		fmt.Println("Diff Scheduling Input added is... ", diffAdded.String()) // Test print, delete later
	}
	if len(podDiff.Removed) > 0 || len(snpDiff.Removed) > 0 || len(bindingsDiff.Removed) > 0 || len(itDiff.Removed) > 0 {
		diffRemoved = NewSchedulingInputReconstruct(si.Timestamp, podDiff.Removed, snpDiff.Removed, bindingsDiff.Removed, itDiff.Removed)
		fmt.Println("Diff Scheduling Input removed is... ", diffRemoved.String()) // Test print, delete later
	}
	if len(podDiff.Changed) > 0 || len(snpDiff.Changed) > 0 || len(bindingsDiff.Changed) > 0 || len(itDiff.Changed) > 0 {
		diffChanged = NewSchedulingInputReconstruct(si.Timestamp, podDiff.Changed, snpDiff.Changed, bindingsDiff.Changed, itDiff.Changed)
		fmt.Println("Diff Scheduling Input changed is... ", diffChanged.String()) // Test print, delete later
	}

	return &SchedulingInputDifferences{
		Added:   diffAdded,
		Removed: diffRemoved,
		Changed: diffChanged,
	}
}

// This is the diffPods function which gets the differences between pods
func diffPods(oldPods, newPods []*v1.Pod) PodDifferences {
	diff := PodDifferences{
		Added:   []*v1.Pod{},
		Removed: []*v1.Pod{},
		Changed: []*v1.Pod{},
	}

	// Reference each pod by its UID
	oldPodSet := map[string]*v1.Pod{}
	for _, pod := range oldPods {
		oldPodSet[string(pod.GetUID())] = pod
	}

	newPodSet := map[string]*v1.Pod{}
	for _, pod := range newPods {
		newPodSet[string(pod.GetUID())] = pod
	}

	// Find the added and changed pods
	for _, newPod := range newPods {
		oldPod, exists := oldPodSet[string(newPod.GetUID())]

		if !exists {
			diff.Added = append(diff.Added, newPod)
		} else if hasPodChanged(oldPod, newPod) {
			// If pod has changed, add the whole changed pod
			// Simplification / Opportunity to optimize -- Only add sub-field.
			//    This requires more book-keeping on object reconstruction from logs later on.
			diff.Changed = append(diff.Changed, newPod)
		}
	}

	// Find the removed pods
	for _, oldPod := range oldPods {
		if _, exists := newPodSet[string(oldPod.GetUID())]; !exists {
			diff.Removed = append(diff.Removed, oldPod)
		}
	}

	return diff
}

// This is the diffStateNodes function which gets the differences between statenodes
func diffStateNodes(oldStateNodesWithPods, newStateNodesWithPods []*StateNodeWithPods) SNPDifferences {
	diff := SNPDifferences{
		Added:   []*StateNodeWithPods{},
		Removed: []*StateNodeWithPods{},
		Changed: []*StateNodeWithPods{},
	}

	// Reference StateNodesWithPods by its name
	oldStateNodeSet := map[string]*StateNodeWithPods{}
	for _, stateNodeWithPods := range oldStateNodesWithPods {
		oldStateNodeSet[stateNodeWithPods.GetName()] = stateNodeWithPods
	}

	newStateNodeSet := map[string]*StateNodeWithPods{}
	for _, stateNodeWithPods := range newStateNodesWithPods {
		newStateNodeSet[stateNodeWithPods.GetName()] = stateNodeWithPods
	}

	// Find the added and changed StateNodesWithPods
	for _, newStateNodeWithPods := range newStateNodesWithPods {
		oldStateNodeWithPods, exists := oldStateNodeSet[newStateNodeWithPods.GetName()]

		if !exists { // Same opportunity for optimization as in pods
			diff.Added = append(diff.Added, newStateNodeWithPods)
		} else if hasStateNodeWithPodsChanged(oldStateNodeWithPods, newStateNodeWithPods) {
			diff.Changed = append(diff.Changed, newStateNodeWithPods)
		}
	}

	// Find the removed StateNodesWithPods
	for _, oldStateNodeWithPods := range oldStateNodesWithPods {
		if _, exists := newStateNodeSet[oldStateNodeWithPods.GetName()]; !exists {
			diff.Removed = append(diff.Removed, oldStateNodeWithPods)
		}
	}

	return diff
}

func diffBindings(old, new map[types.NamespacedName]string) BindingDifferences {
	diff := BindingDifferences{
		Added:   map[types.NamespacedName]string{},
		Removed: map[types.NamespacedName]string{},
		Changed: map[types.NamespacedName]string{},
	}

	// Find the changed or removed bindings
	for k, v := range old {
		if newVal, ok := new[k]; ok {
			if v != newVal {
				diff.Changed[k] = newVal
			}
		} else {
			diff.Removed[k] = v
		}
	}

	// Find the added bindings
	for k, v := range new {
		if _, ok := old[k]; !ok {
			diff.Added[k] = v
		}
	}

	return diff
}

// This is the diffInstanceTypes function which gets the differences between instance types
func diffInstanceTypes(oldTypes, newTypes []*cloudprovider.InstanceType) InstanceTypeDifferences {
	diff := InstanceTypeDifferences{
		Added:   []*cloudprovider.InstanceType{},
		Removed: []*cloudprovider.InstanceType{},
		Changed: []*cloudprovider.InstanceType{},
	}

	// Reference InstanceTypes by their Name
	oldTypeSet := map[string]*cloudprovider.InstanceType{}
	for _, instanceType := range oldTypes {
		oldTypeSet[instanceType.Name] = instanceType
	}

	newTypeSet := map[string]*cloudprovider.InstanceType{}
	for _, instanceType := range newTypes {
		newTypeSet[instanceType.Name] = instanceType
	}

	// Find the added and changed instance types
	for _, newType := range newTypes {
		oldType, exists := oldTypeSet[newType.Name]

		if !exists {
			diff.Added = append(diff.Added, newType)
		} else if hasInstanceTypeChanged(oldType, newType) {
			diff.Changed = append(diff.Changed, newType)
		}
	}

	// Find the removed instance types
	for _, oldType := range oldTypes {
		if _, exists := newTypeSet[oldType.Name]; !exists {
			diff.Removed = append(diff.Removed, oldType)
		}
	}

	return diff
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
		!structEqualJSON(oldInstanceType.Offerings, newInstanceType.Offerings) || // Cannot deep equal these, they have unexported types
		!structEqualJSON(oldInstanceType.Requirements, newInstanceType.Requirements)
}

// TODO: Likely inefficient equality checking for nested types Offerings and Requirements,
// but both have unexported types not compatible with DeepEqual
func structEqualJSON(a, b interface{}) bool {
	aBytes, _ := json.Marshal(a)
	bBytes, _ := json.Marshal(b)
	return bytes.Equal(aBytes, bBytes)
}
