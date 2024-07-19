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

// A stateNode with the Pods it has on it.
type StateNodeWithPods struct {
	Node      *v1.Node
	NodeClaim *v1beta1.NodeClaim
	Pods      []*v1.Pod
}

// Construct and reduce the Scheduling Input down to what's minimally required for re-simulation
func NewSchedulingInput(ctx context.Context, kubeClient client.Client, scheduledTime time.Time,
	pendingPods []*v1.Pod, stateNodes []*state.StateNode, instanceTypes []*cloudprovider.InstanceType) SchedulingInput {
	return SchedulingInput{
		Timestamp:          scheduledTime,
		PendingPods:        reducePods(pendingPods),
		StateNodesWithPods: newStateNodesWithPods(ctx, kubeClient, stateNodes),
		InstanceTypes:      reduceInstanceTypes(instanceTypes),
	}
}

func (snp StateNodeWithPods) GetName() string {
	if snp.Node == nil {
		return snp.NodeClaim.GetName()
	}
	return snp.Node.GetName()
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
			},
			Status: v1.PodStatus{
				Phase: pod.Status.Phase,
			},
		}
		fmt.Println("New ReducedPod: ", reducedPod.String())
		fmt.Println("My ReducedPod: ", PodToString(reducedPod))
		reducedPods = append(reducedPods, reducedPod)
	}
	return reducedPods
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

func MarshalDifferences(DiffAdded *SchedulingInput, DiffRemoved *SchedulingInput, DiffChanged *SchedulingInput) ([]byte, error) {
	return proto.Marshal(&pb.Differences{
		Added:   protoSchedulingInput(DiffAdded),
		Removed: protoSchedulingInput(DiffRemoved),
		Changed: protoSchedulingInput(DiffChanged),
	})
}

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
