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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pb "sigs.k8s.io/karpenter/pkg/controllers/orb/proto"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	v1 "k8s.io/api/core/v1"
)

func protoSchedulingInput(si *SchedulingInput) *pb.SchedulingInput {
	return &pb.SchedulingInput{
		Timestamp:         si.Timestamp.Format("2006-01-02_15-04-05"),
		PendingpodData:    protoPods(si.PendingPods),
		StatenodesData:    protoStateNodesWithPods(si.StateNodesWithPods),
		InstancetypesData: protoInstanceTypes(si.InstanceTypes),
	}
}

func reconstructSchedulingInput(pbsi *pb.SchedulingInput) (*SchedulingInput, error) {
	timestamp, err := time.Parse("2006-01-02_15-04-05", pbsi.GetTimestamp())
	if err != nil {
		return nil, fmt.Errorf("failed to parse timestamp: %v", err)
	}

	return &SchedulingInput{
		Timestamp:          timestamp,
		PendingPods:        reconstructPods(pbsi.GetPendingpodData()),
		StateNodesWithPods: reconstructStateNodesWithPods(pbsi.GetStatenodesData()),
		InstanceTypes:      reconstructInstanceTypes(pbsi.GetInstancetypesData()),
	}, nil
}

func protoPods(pods []*v1.Pod) []*pb.ReducedPod {
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

func reconstructPods(reducedPods []*pb.ReducedPod) []*v1.Pod {
	pods := []*v1.Pod{}
	for _, reducedPod := range reducedPods {
		pods = append(pods, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      reducedPod.Name,
				Namespace: reducedPod.Namespace,
			},
			Status: v1.PodStatus{
				Phase: v1.PodPhase(reducedPod.Phase),
			},
		})
	}
	return pods
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
			Key:                  "", //requirement.Key,
			Nodeselectoroperator: "", //string(requirement.Operator()),
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
