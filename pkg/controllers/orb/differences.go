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
	"reflect"
	"sort"
	"time"

	"github.com/samber/lo"
	"google.golang.org/protobuf/proto"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pb "sigs.k8s.io/karpenter/pkg/controllers/orb/proto"

	v1 "k8s.io/api/core/v1"
)

// A resource's Differences are three versions of that same resource: what has been Added, Removed or Changed.
type Differences[T any] struct {
	Added   T
	Removed T
	Changed T
}

type SchedulingInputDifferences Differences[*SchedulingInput]

// Retrieves the time from any non-empty scheduling input in differences. The times are equivalent between them.
func (differences *SchedulingInputDifferences) GetTimestamp() time.Time {
	if differences.Added != nil && !differences.Added.Timestamp.IsZero() {
		return differences.Added.Timestamp
	} else if differences.Removed != nil && !differences.Removed.Timestamp.IsZero() {
		return differences.Removed.Timestamp
	} else if differences.Changed != nil && !differences.Changed.Timestamp.IsZero() {
		return differences.Changed.Timestamp
	}
	return time.Time{} // Default the zero value of time.Time if no timestamp is found
}

func (differences *SchedulingInputDifferences) getByteSize() int {
	differencesData, err := MarshalBatchedDifferences([]*SchedulingInputDifferences{differences})
	if err != nil {
		return 0
	}
	return len(differencesData)
}

// Gets the time window from a slice of differences, from the start to end timestamp.
func GetTimeWindow(differences []*SchedulingInputDifferences) (time.Time, time.Time) {
	start := time.Time{}
	end := time.Time{}
	for _, diff := range differences {
		timestamp := diff.GetTimestamp()
		if start.IsZero() || timestamp.Before(start) {
			start = timestamp
		}
		if end.IsZero() || timestamp.After(end) {
			end = timestamp
		}
	}
	return start, end
}

// Pulls the cross-sectional slices (added, removed, changed) of each Scheduling Input Differences
func crossSection(differences []*SchedulingInputDifferences) ([]*SchedulingInput, []*SchedulingInput, []*SchedulingInput) {
	allAdded := []*SchedulingInput{}
	allRemoved := []*SchedulingInput{}
	allChanged := []*SchedulingInput{}

	for _, diff := range differences {
		if diff.Added != nil {
			allAdded = append(allAdded, diff.Added)
		}
		if diff.Removed != nil {
			allRemoved = append(allRemoved, diff.Removed)
		}
		if diff.Changed != nil {
			allChanged = append(allChanged, diff.Changed)
		}
	}
	return allAdded, allRemoved, allChanged
}

// Pulls out cross-sectional maps of each Scheduling Input Difference mapped by their timestamp,
// returned alongside the corresponding sorted slice of times for that batch, from oldest to most recent.
func crossSectionByTimestamp(differences []*SchedulingInputDifferences) (map[time.Time]*SchedulingInput, map[time.Time]*SchedulingInput, map[time.Time]*SchedulingInput, []time.Time) {
	allAdded := map[time.Time]*SchedulingInput{}
	allRemoved := map[time.Time]*SchedulingInput{}
	allChanged := map[time.Time]*SchedulingInput{}
	batchedTimes := []time.Time{}

	for _, diff := range differences {
		// Map the Timestamp to its corresponding Difference
		if diff.Added != nil && !diff.Added.Timestamp.IsZero() {
			allAdded[diff.Added.Timestamp] = diff.Added
		}
		if diff.Removed != nil && !diff.Removed.Timestamp.IsZero() {
			allRemoved[diff.Removed.Timestamp] = diff.Removed
		}
		if diff.Changed != nil && !diff.Changed.Timestamp.IsZero() {
			allChanged[diff.Changed.Timestamp] = diff.Changed
		}
		// Batch all Differences' timestamps together
		batchedTimes = append(batchedTimes, diff.GetTimestamp())
	}

	sort.Slice(batchedTimes, func(i, j int) bool { return batchedTimes[i].Before(batchedTimes[j]) })
	return allAdded, allRemoved, allChanged, batchedTimes
}

func MarshalBatchedDifferences(batchedDifferences []*SchedulingInputDifferences) ([]byte, error) {
	allAdded, allRemoved, allChanged := crossSection(batchedDifferences)
	protoDifferences := &pb.BatchedDifferences{
		Added:   protoSchedulingInputs(allAdded),
		Removed: protoSchedulingInputs(allRemoved),
		Changed: protoSchedulingInputs(allChanged),
	}
	protoData, err := proto.Marshal(protoDifferences)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Differences: %v", err)
	}
	return protoData, nil
}

func UnmarshalBatchedDifferences(differencesData []byte) ([]*SchedulingInputDifferences, error) {
	batchedDifferences := &pb.BatchedDifferences{}
	if err := proto.Unmarshal(differencesData, batchedDifferences); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Differences: %v", err)
	}

	batchedAdded, err := reconstructSchedulingInputs(batchedDifferences.GetAdded())
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct Added: %v", err)
	}
	batchedRemoved, err := reconstructSchedulingInputs(batchedDifferences.GetRemoved())
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct Removed: %v", err)
	}
	batchedChanged, err := reconstructSchedulingInputs(batchedDifferences.GetChanged())
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct Changed: %v", err)
	}

	batchedSchedulingInputDifferences := []*SchedulingInputDifferences{}
	for i := 0; i < len(batchedAdded); i++ { // They will have the same dimensionality, even if some are empty
		batchedSchedulingInputDifferences = append(batchedSchedulingInputDifferences, &SchedulingInputDifferences{
			Added:   batchedAdded[i],
			Removed: batchedRemoved[i],
			Changed: batchedChanged[i],
		})
	}

	return batchedSchedulingInputDifferences, nil
}

func protoSchedulingInputs(si []*SchedulingInput) []*pb.SchedulingInput {
	protoSi := []*pb.SchedulingInput{}
	for _, schedulingInput := range si {
		protoSi = append(protoSi, protoSchedulingInput(schedulingInput))
	}
	return protoSi
}

func reconstructSchedulingInputs(pbsi []*pb.SchedulingInput) ([]*SchedulingInput, error) {
	reconstructedSi := []*SchedulingInput{}
	for _, schedulingInput := range pbsi {
		si, err := reconstructSchedulingInput(schedulingInput)
		if err != nil {
			return nil, err
		}
		reconstructedSi = append(reconstructedSi, si)
	}
	return reconstructedSi, nil
}

// Functions to check the differences in all the fields of a SchedulingInput (except the timestamp)
func (oldSi *SchedulingInput) Diff(si *SchedulingInput) *SchedulingInputDifferences {
	podDiff := diffSlice(oldSi.PendingPods, si.PendingPods, getPodKey, hasPodChanged)
	snpDiff := diffSlice(oldSi.StateNodesWithPods, si.StateNodesWithPods, getStateNodeWithPodsKey, hasStateNodeWithPodsChanged)
	bindingsDiff := diffMap(oldSi.Bindings, si.Bindings, hasBindingChanged)
	itDiff := diffSlice(oldSi.AllInstanceTypes, si.AllInstanceTypes, GetInstanceTypeKey, hasInstanceTypeChanged)
	npitDiff := diffMap(oldSi.NodePoolInstanceTypes, si.NodePoolInstanceTypes, hasNodePoolInstanceTypeChanged)
	topologyDiff := diffOnlyChanges(oldSi.Topology, si.Topology)
	dspDiff := diffSlice(oldSi.DaemonSetPods, si.DaemonSetPods, getPodKey, hasPodChanged)
	pvListDiff := diffOnlyChanges(oldSi.PVList, si.PVList)
	pvcListDiff := diffOnlyChanges(oldSi.PVCList, si.PVCList)
	scheduledPodListDiff := diffOnlyChanges(oldSi.ScheduledPodList, si.ScheduledPodList)

	// Added and Removed is not defined for unitary resources (topology, pvList, pvcList and scheduledPodList).
	diffAdded := &SchedulingInput{si.Timestamp, podDiff.Added, snpDiff.Added, bindingsDiff.Added, itDiff.Added, npitDiff.Added, nil, dspDiff.Added, nil, nil, nil}
	diffRemoved := &SchedulingInput{si.Timestamp, podDiff.Removed, snpDiff.Removed, bindingsDiff.Removed, itDiff.Removed, npitDiff.Removed, nil, dspDiff.Removed, nil, nil, nil}
	diffChanged := &SchedulingInput{si.Timestamp, podDiff.Changed, snpDiff.Changed, bindingsDiff.Changed, itDiff.Changed, npitDiff.Changed, topologyDiff.Changed, dspDiff.Changed, pvListDiff.Changed, pvcListDiff.Changed, scheduledPodListDiff.Changed}

	if diffAdded.isEmpty() {
		diffAdded = &SchedulingInput{}
	}
	if diffRemoved.isEmpty() {
		diffRemoved = &SchedulingInput{}
	}
	if diffChanged.isEmpty() {
		diffChanged = &SchedulingInput{}
	}

	return &SchedulingInputDifferences{
		Added:   diffAdded,
		Removed: diffRemoved,
		Changed: diffChanged,
	}
}

func diffSlice[T any](oldresource, newresource []*T, getKey func(*T) string, hasChanged func(*T, *T) bool) Differences[[]*T] {
	added := []*T{}
	removed := []*T{}
	changed := []*T{}

	oldResourceMap := CreateMapFromSlice(oldresource, getKey)
	newResourceMap := CreateMapFromSlice(newresource, getKey)
	oldResourceSet := sets.KeySet(oldResourceMap)
	newResourceSet := sets.KeySet(newResourceMap)

	for addedKey := range newResourceSet.Difference(oldResourceSet) {
		added = append(added, newResourceMap[addedKey])
	}

	for removedKey := range oldResourceSet.Difference(newResourceSet) {
		removed = append(removed, oldResourceMap[removedKey])
	}

	for commonKey := range oldResourceSet.Intersection(newResourceSet) {
		if hasChanged(oldResourceMap[commonKey], newResourceMap[commonKey]) {
			changed = append(changed, newResourceMap[commonKey])
		}
	}
	return Differences[[]*T]{Added: added, Removed: removed, Changed: changed}
}

func diffMap[K comparable, V any](oldResourceMap, newResourceMap map[K]V, hasChanged func(V, V) bool) Differences[map[K]V] {
	added := map[K]V{}
	removed := map[K]V{}
	changed := map[K]V{}

	for key, resource := range oldResourceMap {
		if newValue, ok := newResourceMap[key]; ok {
			if hasChanged(resource, newValue) {
				changed[key] = newValue
			}
		} else {
			removed[key] = resource
		}
	}

	for key, resource := range newResourceMap {
		if _, ok := oldResourceMap[key]; !ok {
			added[key] = resource
		}
	}
	return Differences[map[K]V]{Added: added, Removed: removed, Changed: changed}
}

func diffOnlyChanges[T any](oldresource, newresource *T) Differences[*T] {
	if !structEqualJSON(oldresource, newresource) {
		return Differences[*T]{nil, nil, newresource}
	}
	return Differences[*T]{nil, nil, oldresource}
}

/* Equality functions for 'hasChanged' lambda functions */

func hasPodChanged(oldPod, newPod *v1.Pod) bool {
	return !equality.Semantic.DeepEqual(oldPod.ObjectMeta, newPod.ObjectMeta) ||
		!equality.Semantic.DeepEqual(oldPod.Status, newPod.Status) ||
		!equality.Semantic.DeepEqual(oldPod.Spec, newPod.Spec)
}

func hasStateNodeWithPodsChanged(oldStateNodeWithPods, newStateNodeWithPods *StateNodeWithPods) bool {
	return !equality.Semantic.DeepEqual(oldStateNodeWithPods, newStateNodeWithPods)
}

func hasBindingChanged(oldBinding, newBinding string) bool {
	return oldBinding != newBinding
}

func hasInstanceTypeChanged(oldInstanceType, newInstanceType *cloudprovider.InstanceType) bool {
	return !equality.Semantic.DeepEqual(oldInstanceType.Name, newInstanceType.Name) ||
		!structEqualJSON(oldInstanceType.Offerings, newInstanceType.Offerings) ||
		!structEqualJSON(oldInstanceType.Requirements, newInstanceType.Requirements) ||
		!structEqualJSON(oldInstanceType.Capacity, newInstanceType.Capacity) ||
		!structEqualJSON(oldInstanceType.Overhead, newInstanceType.Overhead)
}

func hasNodePoolInstanceTypeChanged(instancetypes, newInstanceTypes []string) bool {
	return !equality.Semantic.DeepEqual(sets.NewString(instancetypes...), sets.NewString(newInstanceTypes...))
}

// Used when fields contain unexported types, which would cause DeepEqual to panic.
func structEqualJSON(a, b interface{}) bool {
	aBytes, _ := json.Marshal(a)
	bBytes, _ := json.Marshal(b)
	return bytes.Equal(aBytes, bBytes)
}

// Reconstructs a Scheduling Input at a desired reconstruct time by iteratively adding back all logged Differences onto the baseline Scheduling Input up until that time.
func MergeDifferences(baseline *SchedulingInput, batchedDifferences []*SchedulingInputDifferences, reconstructTime time.Time) *SchedulingInput {
	batchedAdded, batchedRemoved, batchedChanged, sortedBatchedTimes := crossSectionByTimestamp(batchedDifferences)

	mergingInputs := &SchedulingInput{
		Timestamp:             reconstructTime,
		PendingPods:           baseline.PendingPods,
		StateNodesWithPods:    baseline.StateNodesWithPods,
		Bindings:              baseline.Bindings,
		AllInstanceTypes:      baseline.AllInstanceTypes,
		NodePoolInstanceTypes: baseline.NodePoolInstanceTypes,
		Topology:              baseline.Topology,
		DaemonSetPods:         baseline.DaemonSetPods,
		PVList:                baseline.PVList,
		PVCList:               baseline.PVCList,
		ScheduledPodList:      baseline.ScheduledPodList,
	}

	for _, differencesTime := range sortedBatchedTimes {
		if differencesTime.After(reconstructTime) {
			break
		}
		mergeSchedulingInputs(mergingInputs, &SchedulingInputDifferences{
			Added:   batchedAdded[differencesTime],
			Removed: batchedRemoved[differencesTime],
			Changed: batchedChanged[differencesTime],
		})
	}
	return mergingInputs
}

// Merges one time's set of Differences on the baseline/merging Inputs; adding, removing and changing each field as appropriate.
func mergeSchedulingInputs(iteratingInput *SchedulingInput, differences *SchedulingInputDifferences) {
	iteratingInput.PendingPods = merge(iteratingInput.PendingPods, differences, GetPendingPods, getPodKey)
	iteratingInput.StateNodesWithPods = merge(iteratingInput.StateNodesWithPods, differences, GetStateNodesWithPods, getStateNodeWithPodsKey)
	mergeMap(iteratingInput.Bindings, differences, GetBindings)
	iteratingInput.AllInstanceTypes = merge(iteratingInput.AllInstanceTypes, differences, GetAllInstanceTypes, GetInstanceTypeKey)
	mergeMap(iteratingInput.NodePoolInstanceTypes, differences, GetNodePoolInstanceTypes)
	iteratingInput.Topology = mergeOnlyChanged(iteratingInput.Topology, differences, GetTopology)
	iteratingInput.DaemonSetPods = merge(iteratingInput.DaemonSetPods, differences, GetDaemonSetPods, getPodKey)
	iteratingInput.PVList = mergeOnlyChanged(iteratingInput.PVList, differences, GetPVList)
	iteratingInput.PVCList = mergeOnlyChanged(iteratingInput.PVCList, differences, GetPVCList)
	iteratingInput.ScheduledPodList = mergeOnlyChanged(iteratingInput.ScheduledPodList, differences, GetScheduledPodList)
}

// Generalized function to merge in the Differences into each different Scheduling Input field.
func merge[T any](items []*T, differences *SchedulingInputDifferences, getResource func(*SchedulingInput) []*T, getKey func(item *T) string) []*T {
	mergedMap := CreateMapFromSlice(items, getKey)

	if differences.Added != nil {
		for _, addedItem := range getResource(differences.Added) {
			mergedMap[getKey(addedItem)] = addedItem
		}
	}
	if differences.Removed != nil {
		for _, removedItem := range getResource(differences.Removed) {
			delete(mergedMap, getKey(removedItem))
		}
	}
	if differences.Changed != nil {
		for _, changedItem := range getResource(differences.Changed) {
			mergedMap[getKey(changedItem)] = changedItem
		}
	}

	mergedItems := []*T{}
	for _, mergedItem := range mergedMap {
		mergedItems = append(mergedItems, mergedItem)
	}
	return mergedItems
}

// Generalized function to merge Differences in place in a map.
func mergeMap[K comparable, V any](m map[K]V, differences *SchedulingInputDifferences, getResource func(*SchedulingInput) map[K]V) {
	if differences.Added != nil {
		for k, v := range getResource(differences.Added) {
			m[k] = v
		}
	}
	if differences.Removed != nil {
		for k := range getResource(differences.Removed) {
			delete(m, k)
		}
	}
	if differences.Changed != nil {
		for k, v := range getResource(differences.Changed) {
			m[k] = v
		}
	}
}

// Some fields of Scheduling Inputs aren't slices or maps, and thus aren't defined for additions or removals, only for changes.
// This function handles those cases by merging the changed field into the iterating field if the changed field is non-empty.
func mergeOnlyChanged[T any](original *T, differences *SchedulingInputDifferences, getResource func(*SchedulingInput) *T) *T {
	if differences.Changed != nil {
		difference := getResource(differences.Changed)
		if difference != nil && !reflect.DeepEqual(*difference, reflect.Zero(reflect.TypeOf(*difference)).Interface()) { // Checks emptiness
			return difference
		}
	}
	return original
}

func CreateMapFromSlice[T any, K comparable](slice []*T, getKey func(*T) K) map[K]*T {
	return lo.Associate(slice, func(item *T) (K, *T) { return getKey(item), item })
}

// Aliases for the getKey() functions to pass into merge
func getPodKey(pod *v1.Pod) string                             { return string(pod.GetUID()) }
func getStateNodeWithPodsKey(snwp *StateNodeWithPods) string   { return snwp.GetName() }
func GetInstanceTypeKey(it *cloudprovider.InstanceType) string { return it.Name }
