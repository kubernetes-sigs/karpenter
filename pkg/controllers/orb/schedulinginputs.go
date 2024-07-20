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
	"context"
	"fmt"
	"time"

	proto "google.golang.org/protobuf/proto"
	// proto "github.com/gogo/protobuf/proto"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	Bindings           map[types.NamespacedName]string
	InstanceTypes      []*cloudprovider.InstanceType
	// TODO: all the other scheduling inputs... (bindings?)
}

// A stateNode with the Pods it has on it.
type StateNodeWithPods struct {
	Node      *v1.Node
	NodeClaim *v1beta1.NodeClaim
	Pods      []*v1.Pod
}

// Construct and reduce the Scheduling Input down to what's minimally required for re-simulation
func NewSchedulingInput(ctx context.Context, kubeClient client.Client, scheduledTime time.Time, pendingPods []*v1.Pod,
	stateNodes []*state.StateNode, bindings map[types.NamespacedName]string, instanceTypes []*cloudprovider.InstanceType) SchedulingInput {
	return SchedulingInput{
		Timestamp:          scheduledTime,
		PendingPods:        pendingPods,
		StateNodesWithPods: newStateNodesWithPods(ctx, kubeClient, stateNodes),
		Bindings:           bindings,
		InstanceTypes:      instanceTypes,
	}
}

func NewSchedulingInputReconstruct(timestamp time.Time, pendingPods []*v1.Pod, stateNodesWithPods []*StateNodeWithPods,
	bindings map[types.NamespacedName]string, instanceTypes []*cloudprovider.InstanceType) *SchedulingInput {
	return &SchedulingInput{
		Timestamp:          timestamp,
		PendingPods:        pendingPods,
		StateNodesWithPods: stateNodesWithPods,
		Bindings:           bindings,
		InstanceTypes:      instanceTypes,
	}
}

func (snp StateNodeWithPods) GetName() string {
	if snp.Node == nil {
		return snp.NodeClaim.GetName()
	}
	return snp.Node.GetName()
}

func (si *SchedulingInput) Reduce() {
	si.PendingPods = reducePods(si.PendingPods)
	si.InstanceTypes = reduceInstanceTypes(si.InstanceTypes)
}

func (si SchedulingInput) String() string {
	return protoSchedulingInput(&si).String()
}

func newStateNodesWithPods(ctx context.Context, kubeClient client.Client, stateNodes []*state.StateNode) []*StateNodeWithPods {
	stateNodesWithPods := []*StateNodeWithPods{}
	for _, stateNode := range reduceStateNodes(stateNodes) {
		pods, err := stateNode.Pods(ctx, kubeClient)
		if err != nil {
			pods = nil
		}

		stateNodesWithPods = append(stateNodesWithPods, &StateNodeWithPods{
			Node:      stateNode.Node,
			NodeClaim: stateNode.NodeClaim,
			Pods:      reducePods(pods),
		})
	}
	return stateNodesWithPods
}

/* Functions to reduce resources in Scheduling Inputs to the constituent parts we care to log / introspect */

func reducePods(pods []*v1.Pod) []*v1.Pod {
	reducedPods := []*v1.Pod{}
	for _, pod := range pods {
		reducedPod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
				UID:       pod.GetUID(),
			},
			Status: v1.PodStatus{
				Phase:      pod.Status.Phase,
				Conditions: reducePodConditions(pod.Status.Conditions),
			},
		}
		reducedPods = append(reducedPods, reducedPod)
	}
	return reducedPods
}

func reducePodConditions(conditions []v1.PodCondition) []v1.PodCondition {
	reducedConditions := []v1.PodCondition{}
	for _, condition := range conditions {
		reducedCondition := v1.PodCondition{
			Type:    condition.Type,
			Status:  condition.Status,
			Reason:  condition.Reason,
			Message: condition.Message,
		}
		reducedConditions = append(reducedConditions, reducedCondition)
	}
	return reducedConditions
}

func reduceStateNodes(nodes []*state.StateNode) []*state.StateNode {
	reducedStateNodes := []*state.StateNode{}
	for _, node := range nodes {
		if node != nil {
			reducedStateNode := &state.StateNode{}
			if node.Node != nil {
				reducedStateNode.Node = &v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node.Node.Name,
					},
					Status: node.Node.Status,
				}
			}
			if node.NodeClaim != nil {
				reducedStateNode.NodeClaim = &v1beta1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: node.NodeClaim.Name,
					},
				}
			}
			if reducedStateNode.Node != nil || reducedStateNode.NodeClaim != nil {
				reducedStateNodes = append(reducedStateNodes, reducedStateNode)
			}
		}
	}
	return reducedStateNodes
}

func reduceOfferings(offerings cloudprovider.Offerings) cloudprovider.Offerings {
	strippedOfferings := cloudprovider.Offerings{}
	for _, offering := range offerings {
		strippedOffering := &cloudprovider.Offering{
			Requirements: reduceRequirements(offering.Requirements),
			Price:        offering.Price,
			Available:    offering.Available,
		}
		strippedOfferings = append(strippedOfferings, *strippedOffering)
	}
	return strippedOfferings
}

// Reduce Requirements returns Requirements of these keys: karpenter.sh/capacity-type, topology.k8s.aws/zone-id and topology.kubernetes.io/zone
// TODO Should these keys be called more generically? i.e. via v1beta1.CapacityTypeLabelKey, v1.LabelTopologyZone or something?
func reduceRequirements(requirements scheduling.Requirements) scheduling.Requirements {
	reducedRequirements := scheduling.Requirements{}
	for key, value := range requirements {
		switch key {
		case "karpenter.sh/capacity-type", "topology.k8s.aws/zone-id", "topology.kubernetes.io/zone":
			reducedRequirements[key] = value
		}
	}
	return reducedRequirements
}

func reduceInstanceTypes(its []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	reducedInstanceTypes := []*cloudprovider.InstanceType{}
	for _, it := range its {
		reducedInstanceType := &cloudprovider.InstanceType{
			Name:         it.Name,
			Requirements: reduceRequirements(it.Requirements),
			Offerings:    reduceOfferings(it.Offerings.Available()),
		}
		reducedInstanceTypes = append(reducedInstanceTypes, reducedInstanceType)
	}
	return reducedInstanceTypes
}

/* Functions to convert between SchedulingInputs and the proto-defined version
   Via pairs: Marshal <--> Unmarshal and proto <--> reconstruct */

func MarshalSchedulingInput(si *SchedulingInput) ([]byte, error) {
	return proto.Marshal(protoSchedulingInput(si))
}

func UnmarshalSchedulingInput(schedulingInputData []byte) (*SchedulingInput, error) {
	entry := &pb.SchedulingInput{}

	if err := proto.Unmarshal(schedulingInputData, entry); err != nil {
		return nil, fmt.Errorf("failed to unmarshal SchedulingInput: %v", err)
	}

	si, err := reconstructSchedulingInput(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct SchedulingInput: %v", err)
	}
	return si, nil
}

func protoSchedulingInput(si *SchedulingInput) *pb.SchedulingInput {
	return &pb.SchedulingInput{
		Timestamp:         si.Timestamp.Format("2006-01-02_15-04-05"),
		PendingpodData:    protoPods(si.PendingPods),
		BindingsData:      protoBindings(si.Bindings),
		StatenodesData:    protoStateNodesWithPods(si.StateNodesWithPods),
		InstancetypesData: protoInstanceTypes(si.InstanceTypes),
	}
}

func reconstructSchedulingInput(pbsi *pb.SchedulingInput) (*SchedulingInput, error) {
	timestamp, err := time.Parse("2006-01-02_15-04-05", pbsi.GetTimestamp())
	if err != nil {
		return nil, fmt.Errorf("failed to parse timestamp: %v", err)
	}

	return NewSchedulingInputReconstruct(
		timestamp,
		reconstructPods(pbsi.GetPendingpodData()),
		reconstructStateNodesWithPods(pbsi.GetStatenodesData()),
		reconstructBindings(pbsi.GetBindingsData()),
		reconstructInstanceTypes(pbsi.GetInstancetypesData()),
	), nil
}

func protoPods(pods []*v1.Pod) []*pb.ReducedPod {
	reducedPods := []*pb.ReducedPod{}
	for _, pod := range pods {
		reducedPod := &pb.ReducedPod{
			Name:       pod.Name,
			Namespace:  pod.Namespace,
			Uid:        string(pod.GetUID()),
			Phase:      string(pod.Status.Phase),
			Conditions: protoPodConditions(pod.Status.Conditions),
		}
		reducedPods = append(reducedPods, reducedPod)
	}
	return reducedPods
}

func reconstructPods(reducedPods []*pb.ReducedPod) []*v1.Pod {
	pods := []*v1.Pod{}
	for _, reducedPod := range reducedPods {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      reducedPod.Name,
				Namespace: reducedPod.Namespace,
				UID:       types.UID(reducedPod.Uid),
			},
			Status: v1.PodStatus{
				Phase:      v1.PodPhase(reducedPod.Phase),
				Conditions: reconstructPodConditions(reducedPod.Conditions),
			},
		}
		pods = append(pods, pod)
	}
	return pods
}

func protoPodConditions(conditions []v1.PodCondition) []*pb.ReducedPod_PodCondition {
	reducedPodConditions := []*pb.ReducedPod_PodCondition{}
	for _, condition := range conditions {
		reducedPodCondition := &pb.ReducedPod_PodCondition{
			Type:    string(condition.Type),
			Status:  string(condition.Status),
			Reason:  condition.Reason,
			Message: condition.Message,
		}
		reducedPodConditions = append(reducedPodConditions, reducedPodCondition)
	}
	return reducedPodConditions
}

func reconstructPodConditions(reducedPodConditions []*pb.ReducedPod_PodCondition) []v1.PodCondition {
	podConditions := []v1.PodCondition{}
	for _, reducedPodCondition := range reducedPodConditions {
		podCondition := v1.PodCondition{
			Type:    v1.PodConditionType(reducedPodCondition.Type),
			Status:  v1.ConditionStatus(reducedPodCondition.Status),
			Reason:  reducedPodCondition.Reason,
			Message: reducedPodCondition.Message,
		}
		podConditions = append(podConditions, podCondition)
	}
	return podConditions
}

func protoStateNodesWithPods(stateNodesWithPods []*StateNodeWithPods) []*pb.StateNodeWithPods {
	snpData := []*pb.StateNodeWithPods{}
	for _, snp := range stateNodesWithPods {
		var nodeClaim *pb.StateNodeWithPods_ReducedNodeClaim
		if snp.NodeClaim != nil {
			nodeClaim = &pb.StateNodeWithPods_ReducedNodeClaim{
				Name: snp.NodeClaim.GetName(),
			}
		}
		snpData = append(snpData, &pb.StateNodeWithPods{
			Node:      protoNode(snp.Node),
			NodeClaim: nodeClaim,
			Pods:      protoPods(snp.Pods),
		})
	}
	return snpData
}

func reconstructStateNodesWithPods(snpData []*pb.StateNodeWithPods) []*StateNodeWithPods {
	stateNodesWithPods := []*StateNodeWithPods{}
	for _, snpData := range snpData {
		var nodeClaim *v1beta1.NodeClaim
		if snpData.NodeClaim != nil {
			nodeClaim = &v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: snpData.NodeClaim.Name,
				},
			}
		}
		stateNodesWithPods = append(stateNodesWithPods, &StateNodeWithPods{
			Node:      reconstructNode(snpData.Node),
			NodeClaim: nodeClaim,
			Pods:      reconstructPods(snpData.Pods),
		})
	}
	return stateNodesWithPods
}

func protoNode(node *v1.Node) *pb.StateNodeWithPods_ReducedNode {
	if node == nil {
		return nil
	}
	nodeStatus, err := node.Status.Marshal()
	if err != nil {
		return nil
	}

	return &pb.StateNodeWithPods_ReducedNode{
		Name:       node.Name,
		Nodestatus: nodeStatus,
	}
}

func reconstructNode(nodeData *pb.StateNodeWithPods_ReducedNode) *v1.Node {
	if nodeData == nil {
		return nil
	}
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeData.Name,
		},
	}
	node.Status.Unmarshal(nodeData.Nodestatus)
	return node
}

func protoInstanceTypes(instanceTypes []*cloudprovider.InstanceType) []*pb.ReducedInstanceType {
	itData := []*pb.ReducedInstanceType{}
	for _, it := range instanceTypes {
		itData = append(itData, &pb.ReducedInstanceType{
			Name:         it.Name,
			Requirements: protoRequirements(it.Requirements),
			Offerings:    protoOfferings(it.Offerings),
		})
	}
	return itData
}

func reconstructInstanceTypes(itData []*pb.ReducedInstanceType) []*cloudprovider.InstanceType {
	instanceTypes := []*cloudprovider.InstanceType{}
	for _, it := range itData {
		instanceTypes = append(instanceTypes, &cloudprovider.InstanceType{
			Name:         it.Name,
			Requirements: reconstructRequirements(it.Requirements),
			Offerings:    reconstructOfferings(it.Offerings),
		})
	}
	return instanceTypes
}

func protoRequirements(requirements scheduling.Requirements) []*pb.ReducedInstanceType_ReducedRequirement {
	requirementsData := []*pb.ReducedInstanceType_ReducedRequirement{}
	for _, requirement := range requirements {
		requirementsData = append(requirementsData, &pb.ReducedInstanceType_ReducedRequirement{
			Key:                  requirement.Key,
			Nodeselectoroperator: string(requirement.Operator()),
			Values:               requirement.Values(),
		})
	}
	return requirementsData
}

func reconstructRequirements(requirementsData []*pb.ReducedInstanceType_ReducedRequirement) scheduling.Requirements {
	requirements := scheduling.Requirements{}
	for _, requirementData := range requirementsData {
		requirements.Add(scheduling.NewRequirement(
			requirementData.Key,
			v1.NodeSelectorOperator(requirementData.Nodeselectoroperator),
			requirementData.Values...,
		))
	}
	return requirements
}

func protoOfferings(offerings cloudprovider.Offerings) []*pb.ReducedInstanceType_ReducedOffering {
	offeringsData := []*pb.ReducedInstanceType_ReducedOffering{}
	for _, offering := range offerings {
		offeringsData = append(offeringsData, &pb.ReducedInstanceType_ReducedOffering{
			Requirements: protoRequirements(offering.Requirements),
			Price:        offering.Price,
			Available:    offering.Available,
		})
	}
	return offeringsData
}

func reconstructOfferings(offeringsData []*pb.ReducedInstanceType_ReducedOffering) cloudprovider.Offerings {
	offerings := cloudprovider.Offerings{}
	for _, offeringData := range offeringsData {
		offerings = append(offerings, cloudprovider.Offering{
			Requirements: reconstructRequirements(offeringData.Requirements),
			Price:        offeringData.Price,
			Available:    offeringData.Available,
		})
	}
	return offerings
}

func protoBindings(bindings map[types.NamespacedName]string) *pb.Bindings {
	bindingsProto := &pb.Bindings{}
	for podNamespacedName, nodeName := range bindings {
		binding := &pb.Bindings_Binding{
			PodNamespacedName: &pb.Bindings_Binding_NamespacedName{
				Namespace: podNamespacedName.Namespace,
				Name:      podNamespacedName.Name,
			},
			NodeName: nodeName,
		}
		bindingsProto.Binding = append(bindingsProto.Binding, binding)
	}
	return bindingsProto
}

func reconstructBindings(bindingsProto *pb.Bindings) map[types.NamespacedName]string {
	bindings := map[types.NamespacedName]string{}
	for _, binding := range bindingsProto.Binding {
		podNamespacedName := types.NamespacedName{
			Namespace: binding.PodNamespacedName.Namespace,
			Name:      binding.PodNamespacedName.Name,
		}
		bindings[podNamespacedName] = binding.NodeName
	}
	return bindings
}
