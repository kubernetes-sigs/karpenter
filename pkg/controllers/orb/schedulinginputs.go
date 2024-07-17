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
	"context"
	"encoding/json"
	"fmt"
	"time"

	// "google.golang.org/protobuf/proto"
	proto "github.com/gogo/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pb "sigs.k8s.io/karpenter/pkg/controllers/orb/proto"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	v1 "k8s.io/api/core/v1"
)

// These are the inputs to the scheduling function (scheduler.NewSchedule) which change more dynamically
type SchedulingInput struct {
	Timestamp          time.Time
	PendingPods        []*v1.Pod
	StateNodesWithPods []*StateNodeWithPods
	InstanceTypes      []*cloudprovider.InstanceType
	// TODO: all the other scheduling inputs... (bindings?)
}

func NewSchedulingInput(ctx context.Context, kubeClient client.Client, scheduledTime time.Time,
	pendingPods []*v1.Pod, stateNodes []*state.StateNode, instanceTypes []*cloudprovider.InstanceType) SchedulingInput {
	return SchedulingInput{
		Timestamp:          scheduledTime,
		PendingPods:        pendingPods,
		StateNodesWithPods: getStateNodesWithPods(ctx, kubeClient, stateNodes),
		InstanceTypes:      instanceTypes,
	}
}

// A stateNode with the Pods it has on it.
type StateNodeWithPods struct {
	Node      *v1.Node
	NodeClaim *v1beta1.NodeClaim
	Pods      []*v1.Pod
}

func getStateNodesWithPods(ctx context.Context, kubeClient client.Client, stateNodes []*state.StateNode) []*StateNodeWithPods {
	stateNodesWithPods := []*StateNodeWithPods{}
	stateNodes = reduceStateNodes(stateNodes)

	for _, stateNode := range stateNodes {
		stateNodesWithPods = append(stateNodesWithPods, getStateNodeWithPods(ctx, kubeClient, stateNode))
	}
	return stateNodesWithPods
}

func getStateNodeWithPods(ctx context.Context, kubeClient client.Client, stateNode *state.StateNode) *StateNodeWithPods {
	pods, err := stateNode.Pods(ctx, kubeClient)
	if err != nil {
		pods = nil
	}

	return &StateNodeWithPods{
		Node:      stateNode.Node,
		NodeClaim: stateNode.NodeClaim,
		Pods:      pods,
	}
}

func (snp StateNodeWithPods) GetName() string {
	if snp.Node == nil {
		return snp.NodeClaim.GetName()
	}
	return snp.Node.GetName()
}

// Reduce the Scheduling Input down to what's minimally required for re-simulation
func (si SchedulingInput) Reduce() SchedulingInput {
	return SchedulingInput{
		Timestamp:          si.Timestamp,
		PendingPods:        reducePods(si.PendingPods),
		StateNodesWithPods: si.StateNodesWithPods,
		InstanceTypes:      reduceInstanceTypes(si.InstanceTypes),
	}
}

// TODO: I need to flip the construct here. I should be generating some stripped/minimal subset of these data structures
// which are already the representation that I'd like to print. i.e. store in memory only what I want to print anyway
func (si SchedulingInput) String() string {
	return fmt.Sprintf("Timestamp (UTC): %v\n\nPendingPods:\n%v\n\nStateNodesWithPods:\n%v\n\nInstanceTypes:\n%v\n\n",
		si.Timestamp.Format("2006-01-02_15-04-05"),
		PodsToString(si.PendingPods),
		StateNodesWithPodsToString(si.StateNodesWithPods),
		InstanceTypesToString(si.InstanceTypes),
	)
}

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
		Timestamp:          si.Timestamp, // i.e. the time of those (newer) differences
		PendingPods:        pendingPodsRemoved,
		StateNodesWithPods: stateNodesRemoved,
		InstanceTypes:      instanceTypesRemoved,
	}

	diffChanged := &SchedulingInput{
		Timestamp:          si.Timestamp, // i.e. the time of those (newer) differences
		PendingPods:        pendingPodsChanged,
		StateNodesWithPods: stateNodesChanged,
		InstanceTypes:      instanceTypesChanged,
	}

	if len(pendingPodsAdded)+len(stateNodesAdded)+len(instanceTypesAdded) == 0 {
		diffAdded = nil
	} else {
		fmt.Println("Diff Scheduling Input added is... ", diffAdded.String()) // Test print, delete later
	}

	if len(pendingPodsRemoved)+len(stateNodesRemoved)+len(instanceTypesRemoved) == 0 {
		diffRemoved = nil
	} else {
		fmt.Println("Diff Scheduling Input removed is... ", diffRemoved.String()) // Test print, delete later
	}

	if len(pendingPodsChanged)+len(stateNodesChanged)+len(instanceTypesChanged) == 0 {
		diffChanged = nil
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

func hasPodChanged(oldPod, newPod *v1.Pod) bool {
	return !equality.Semantic.DeepEqual(oldPod, newPod)
}

func hasStateNodeWithPodsChanged(oldStateNodeWithPods, newStateNodeWithPods *StateNodeWithPods) bool {
	return !equality.Semantic.DeepEqual(oldStateNodeWithPods, newStateNodeWithPods)
}

// Checking equality on only fields I've reduced it to (i.e. Name Requirements Offerings)
func hasInstanceTypeChanged(oldInstanceType, newInstanceType *cloudprovider.InstanceType) bool {
	return !equality.Semantic.DeepEqual(oldInstanceType.Name, newInstanceType.Name) ||
		!equality.Semantic.DeepEqual(oldInstanceType.Offerings, newInstanceType.Offerings) ||
		!structEqual(oldInstanceType.Requirements, newInstanceType.Requirements)
}

// // Equality test for requirements based on the three keys I'm tracking for them, namely
// // karpenter.sh/capacity-type, topology.k8s.aws/zone-id, topology.kubernetes.io/zone and their values
// func requirementsEqual(oldrequirements scheduling.Requirements, newrequirements scheduling.Requirements) bool {
// 	return oldrequirements.Get("karpenter.sh/capacity-type") != newrequirements.Get("karpenter.sh/capacity-type") ||
// 		oldrequirements.Get("topology.k8s.aws/zone-id") != newrequirements.Get("topology.k8s.aws/zone-id") ||
// 		oldrequirements.Get("topology.kubernetes.io/zone") != newrequirements.Get("topology.kubernetes.io/zone")
// }

func structEqual(a, b interface{}) bool {
	aBytes, _ := json.Marshal(a)
	bBytes, _ := json.Marshal(b)
	return bytes.Equal(aBytes, bBytes)
}

/* The following functions are testing toString functions that will mirror what the serialization
   deserialization functions will do in protobuf. These are inefficient, but human-readable */

// TODO: This eventually will be "as simple" as reconstructing the data structures from
// the log data and using K8S and/or Karpenter representation to present as JSON or YAML or something

// This function as a human readable test function for serializing desired pod data
// It takes in a v1.Pod and gets the string representations of all the fields we care about.
func PodToString(pod *v1.Pod) string {
	if pod == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Name: %s, Namespace: %s, Phase: %s}", pod.Name, pod.Namespace, pod.Status.Phase)
}

func PodsToString(pods []*v1.Pod) string {
	if pods == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, pod := range pods {
		buf.WriteString(PodToString(pod) + "\n") // TODO: Can replace with pod.String() if I want/need
	}
	return buf.String()
}

func StateNodeWithPodsToString(node *StateNodeWithPods) string {
	if node == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Node: %s, NodeClaim: %s, {Pods: %s}}",
		NodeToString(node.Node), NodeClaimToString(node.NodeClaim), PodsToString(node.Pods))
}

func StateNodesWithPodsToString(nodes []*StateNodeWithPods) string {
	if nodes == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, node := range nodes {
		buf.WriteString(StateNodeWithPodsToString(node) + "\n")
	}
	return buf.String()
}

// Similar function for human-readable string serialization of a v1.Node
func NodeToString(node *v1.Node) string {
	if node == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Name: %s, Status: %s}", node.Name, node.Status.Phase)
}

// Similar function for NodeClaim
func NodeClaimToString(nodeClaim *v1beta1.NodeClaim) string {
	if nodeClaim == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{NodeClaimName: %s}", nodeClaim.Name)
}

// Similar for instanceTypes (name, requirements, offerings, capacity, overhead
func InstanceTypeToString(instanceType *cloudprovider.InstanceType) string {
	if instanceType == nil {
		return "<nil>"
	}
	// TODO: String print the sub-types, like Offerings, too, all of them
	return fmt.Sprintf("Name: %s,\nRequirements: %s,\nOffering: %s", instanceType.Name,
		RequirementsToString(instanceType.Requirements), OfferingsToString(instanceType.Offerings))
}

func InstanceTypesToString(instanceTypes []*cloudprovider.InstanceType) string {
	if instanceTypes == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, instanceType := range instanceTypes {
		buf.WriteString(InstanceTypeToString(instanceType) + "\n")
	}
	return buf.String()
}

// Similar for IT Requirements
// karpenter.sh/capacity-type In [on-demand spot]
// topology.k8s.aws/zone-id In [usw2-az1 usw2-az2 usw2-az3],
// topology.kubernetes.io/zone In [us-west-2a us-west-2b us-west-2c]
func RequirementsToString(requirements scheduling.Requirements) string {
	if requirements == nil {
		return "<nil>"
	}
	capacityType := requirements.Get("karpenter.sh/capacity-type")
	zoneID := requirements.Get("topology.k8s.aws/zone-id")
	zone := requirements.Get("topology.kubernetes.io/zone")
	return fmt.Sprintf("{%s, %s, %s}", capacityType, zoneID, zone)
}

// Similar for IT Offerings (Price, Availability)
func OfferingToString(offering *cloudprovider.Offering) string {
	if offering == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Requirements: %v, Price: %f, Available: %t}", RequirementsToString(offering.Requirements), offering.Price, offering.Available)
}

func OfferingsToString(offerings cloudprovider.Offerings) string {
	if offerings == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, offering := range offerings {
		buf.WriteString(OfferingToString(&offering) + "\n")
	}
	return buf.String()
}

// Resource reducing commands

// Reduces a Pod to only the constituent parts we care about (i.e. Name, Namespace and Phase)
func reducePods(pods []*v1.Pod) []*v1.Pod {
	var reducedPods []*v1.Pod

	for _, pod := range pods {
		reducedPod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			Status: v1.PodStatus{
				Phase: pod.Status.Phase,
			},
		}
		reducedPods = append(reducedPods, reducedPod)
	}

	return reducedPods
}

func reduceStateNodes(nodes []*state.StateNode) []*state.StateNode {
	var strippedNodes []*state.StateNode

	for _, node := range nodes {
		if node != nil {
			strippedNode := &state.StateNode{}

			if node.Node != nil {
				strippedNode.Node = &v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node.Node.Name,
					},
					Status: node.Node.Status,
				}
			}

			if node.NodeClaim != nil {
				strippedNode.NodeClaim = &v1beta1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: node.NodeClaim.Name,
					},
				}
			}

			if strippedNode.Node != nil || strippedNode.NodeClaim != nil {
				strippedNodes = append(strippedNodes, strippedNode)
			}
		}
	}
	return strippedNodes
}

func reduceOfferings(offerings cloudprovider.Offerings) cloudprovider.Offerings {
	var strippedOfferings cloudprovider.Offerings

	for _, offering := range offerings {
		strippedOffering := &cloudprovider.Offering{
			Requirements: reduceRequirements(offering.Requirements),
			Price:        offering.Price,
			Available:    offering.Available,
		}
		strippedOfferings = append(strippedOfferings, *strippedOffering) // TODO am I handling this pointer dereference right?
	}

	return strippedOfferings
}

// Grab only these key'd values from requirements... karpenter.sh/capacity-type, topology.k8s.aws/zone-id and topology.kubernetes.io/zone
// TODO Should these keys be called more generically? i.e. via v1beta1.CapacityTypeLabelKey, v1.LabelTopologyZone or something?
func reduceRequirements(requirements scheduling.Requirements) scheduling.Requirements {
	// Create a new map to store the reduced requirements
	reducedRequirements := scheduling.Requirements{}

	// Iterate over the requirements map and add the relevant keys and values to the reducedRequirements map
	for key, value := range requirements {
		switch key {
		case "karpenter.sh/capacity-type", "topology.k8s.aws/zone-id", "topology.kubernetes.io/zone":
			reducedRequirements[key] = value
		}
	}

	return reducedRequirements
}

func reduceInstanceTypes(types []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	var reducedInstanceTypes []*cloudprovider.InstanceType

	for _, instanceType := range types {
		reducedInstanceType := &cloudprovider.InstanceType{
			Name:         instanceType.Name,
			Requirements: reduceRequirements(instanceType.Requirements),
			Offerings:    reduceOfferings(instanceType.Offerings.Available()),
		}
		reducedInstanceTypes = append(reducedInstanceTypes, reducedInstanceType)
	}

	return reducedInstanceTypes
}

// Function take a Scheduling Input to []byte, marshalled as a protobuf
func (si SchedulingInput) Marshal() ([]byte, error) {
	preMarshalSI := &pb.SchedulingInput{
		Timestamp:         si.Timestamp.Format("2006-01-02_15-04-05"),
		PendingpodData:    getPodsData(si.PendingPods),
		StatenodesData:    getStateNodeWithPodsData(si.StateNodesWithPods),
		InstancetypesData: getInstanceTypesData(si.InstanceTypes),
	}
	return proto.Marshal(preMarshalSI)
}

func getPodsData(pods []*v1.Pod) []*pb.ReducedPod {
	reducedPods := []*pb.ReducedPod{}

	for _, pod := range pods {
		reducedPod := &pb.ReducedPod{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			Phase:     string(pod.Status.Phase),
		}
		reducedPods = append(reducedPods, reducedPod)
	}

	return reducedPods
}

func getNodeData(node *v1.Node) *pb.StateNodeWithPods_ReducedNode {
	if node == nil {
		return nil
	}

	// nodeStatus, err := node.Status.Marshal()
	// if err != nil {
	// 	return nil
	// }

	// Create a new instance of the reduced node type
	reducedNode := &pb.StateNodeWithPods_ReducedNode{
		Name:       node.Name,
		Nodestatus: nil,
	}

	return reducedNode
}

func getStateNodeWithPodsData(stateNodeWithPods []*StateNodeWithPods) []*pb.StateNodeWithPods {
	snpData := []*pb.StateNodeWithPods{}

	for _, snp := range stateNodeWithPods {
		var nodeClaim *pb.StateNodeWithPods_ReducedNodeClaim
		if snp.NodeClaim != nil {
			nodeClaim = &pb.StateNodeWithPods_ReducedNodeClaim{
				Name: snp.NodeClaim.GetName(),
			}
		}

		snpData = append(snpData, &pb.StateNodeWithPods{
			Node:      getNodeData(snp.Node),
			NodeClaim: nodeClaim,
			Pods:      getPodsData(snp.Pods),
		})
	}

	return snpData
}

func getInstanceTypesData(instanceTypes []*cloudprovider.InstanceType) []*pb.ReducedInstanceType {
	itData := []*pb.ReducedInstanceType{}

	for _, it := range instanceTypes {
		itData = append(itData, &pb.ReducedInstanceType{
			Name:         it.Name,
			Requirements: getRequirementsData(it.Requirements),
			Offerings:    getOfferingsData(it.Offerings),
		})
	}

	return itData
}

func getRequirementsData(requirements scheduling.Requirements) []*pb.ReducedInstanceType_Requirement {
	requirementsData := []*pb.ReducedInstanceType_Requirement{}

	for _, requirement := range requirements {
		requirementsData = append(requirementsData, &pb.ReducedInstanceType_Requirement{
			Values: requirement.Values(),
		})
	}

	return requirementsData
}

func getOfferingsData(offerings cloudprovider.Offerings) []*pb.ReducedInstanceType_Offering {
	offeringsData := []*pb.ReducedInstanceType_Offering{}

	for _, offering := range offerings {
		offeringsData = append(offeringsData, &pb.ReducedInstanceType_Offering{
			Requirements: getRequirementsData(offering.Requirements),
			Price:        offering.Price,
			Available:    offering.Available,
		})
	}

	return offeringsData
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

// // Function to do the reverse, take a scheduling input's []byte and unmarshal it back into a SchedulingInput
// func PBToSchedulingInput(timestamp time.Time, data []byte) (SchedulingInput, error) {
// 	podList := &v1.PodList{}
// 	if err := proto.Unmarshal(data, podList); err != nil {
// 		return SchedulingInput{}, fmt.Errorf("unmarshaling pod list, %w", err)
// 	}
// 	pods := lo.ToSlicePtr(podList.Items)
// 	return NewSchedulingInput(timestamp, pods, nil, nil), nil // TODO: update once I figure out serialization
// }
