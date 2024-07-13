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
	"fmt"
	"time"

	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	//"google.golang.org/protobuf/proto"
	proto "github.com/gogo/protobuf/proto"
	v1 "k8s.io/api/core/v1"
)

// Timestamp, dynamic inputs (like pending pods, statenodes, etc.)
type SchedulingInput struct {
	Timestamp     time.Time
	PendingPods   []*v1.Pod
	StateNodes    []*state.StateNode
	InstanceTypes []*cloudprovider.InstanceType
	//all the other scheduling inputs...
}

func (si SchedulingInput) Reduce() SchedulingInput {
	return SchedulingInput{
		Timestamp:     si.Timestamp,
		PendingPods:   reducePods(si.PendingPods),
		StateNodes:    reduceStateNodes(si.StateNodes),
		InstanceTypes: reduceInstanceTypes(si.InstanceTypes),
	}
}

// TODO: I need to flip the construct here. I should be generating some stripped/minimal subset of these data structures
// which are already the representation that I'd like to print. i.e. store in memory only what I want to print anyway
func (si SchedulingInput) String() string {
	return fmt.Sprintf("Scheduled at Time (UTC): %v\n\nPendingPods:\n%v\n\nStateNodes:\n%v\n\nInstanceTypes:\n%v\n\n",
		si.Timestamp.Format("2006-01-02_15-04-05"),
		PodsToString(si.PendingPods),
		StateNodesToString(si.StateNodes),
		InstanceTypesToString(si.InstanceTypes),
	)
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

func NewSchedulingInput(pendingPods []*v1.Pod, stateNodes []*state.StateNode, instanceTypes []*cloudprovider.InstanceType) SchedulingInput {
	return SchedulingInput{
		Timestamp:     time.Now(),
		PendingPods:   pendingPods,
		StateNodes:    stateNodes,
		InstanceTypes: instanceTypes,
	}
}

// Reconstruct a scheduling input (presumably from a file)
func ReconstructedSchedulingInput(timestamp time.Time, pendingPods []*v1.Pod) SchedulingInput {
	return SchedulingInput{
		Timestamp:   timestamp,
		PendingPods: pendingPods,
	}
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

// Similar function for stateNode
func StateNodeToString(node *state.StateNode) string {
	if node == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Node: %s, NodeClaim: %s}", NodeToString(node.Node), NodeClaimToString(node.NodeClaim))
}

func StateNodesToString(nodes []*state.StateNode) string {
	if nodes == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, node := range nodes {
		buf.WriteString(StateNodeToString(node) + "\n")
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

// InstanceTypes

// Resource reducing commands

// Reduces a Pod to only the constituent parts we care about (i.e. Name, Namespace and Phase)
func reducePods(pods []*v1.Pod) []*v1.Pod {
	var strippedPods []*v1.Pod

	for _, pod := range pods {
		strippedPod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			Status: v1.PodStatus{
				Phase: pod.Status.Phase,
			},
		}
		strippedPods = append(strippedPods, strippedPod)
	}

	return strippedPods
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
	reducedRequirements := make(scheduling.Requirements)

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
	var strippedTypes []*cloudprovider.InstanceType

	for _, instanceType := range types {
		strippedType := &cloudprovider.InstanceType{
			Name:         instanceType.Name,
			Requirements: reduceRequirements(instanceType.Requirements),
			Offerings:    reduceOfferings(instanceType.Offerings.Available()),
		}
		strippedTypes = append(strippedTypes, strippedType)
	}

	return strippedTypes
}
