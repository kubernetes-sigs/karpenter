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

package test

import (
	// "context"
	// "encoding/json"
	// "fmt"
	"context"
	"time"

	// "math/rand"

	// "github.com/imdario/mergo"
	// "github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	// policyv1 "k8s.io/api/policy/v1"
	// "k8s.io/apimachinery/pkg/api/resource"
	// metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// "k8s.io/apimachinery/pkg/util/intstr"
	// proto "google.golang.org/protobuf/proto"
	// "k8s.io/apimachinery/pkg/api/resource"
	// metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// "k8s.io/apimachinery/pkg/util/sets"
	// "sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	// pb "sigs.k8s.io/karpenter/pkg/controllers/orb/proto"
	"sigs.k8s.io/karpenter/pkg/controllers/orb"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	// "sigs.k8s.io/karpenter/pkg/scheduling"
)

// Makes a empty SchedulingInput
func EmptySchedulingInput() *orb.SchedulingInput {
	return &orb.SchedulingInput{}
	// return &orb.SchedulingInput{
	// 	Timestamp:             time.Time{},
	// 	PendingPods:           []*corev1.Pod{},
	// 	StateNodesWithPods:    []*orb.StateNodeWithPods{},
	// 	Bindings:              orb.BindingsMap{},
	// 	AllInstanceTypes:      []*cloudprovider.InstanceType{},
	// 	NodePoolInstanceTypes: map[string][]string{},
	// 	Topology:              nil,
	// 	DaemonSetPods:         []*corev1.Pod{},
	// 	PVList:                nil,
	// 	PVCList:               nil,
	// 	ScheduledPodList:      nil,
	// }
}

// Makes a empty differences of Added, Removed, Changed Scheduling Inputs
func EmptySchedulingInputDifferences() *orb.SchedulingInputDifferences {
	return &orb.SchedulingInputDifferences{
		Added:   EmptySchedulingInput(),
		Removed: EmptySchedulingInput(),
		Changed: EmptySchedulingInput(),
	}
}

func SchedulingInputDifferences(ctx context.Context, envClient client.Client) *orb.SchedulingInputDifferences {
	return &orb.SchedulingInputDifferences{
		Added:   SchedulingInput(ctx, envClient),
		Removed: SchedulingInput(ctx, envClient),
		Changed: SchedulingInput(ctx, envClient),
	}
}

func EmptySchedulingInputDifferencesSlice() []*orb.SchedulingInputDifferences {
	return []*orb.SchedulingInputDifferences{
		EmptySchedulingInputDifferences(),
	}
}

func SchedulingInputDifferencesSlice(ctx context.Context, envClient client.Client) []*orb.SchedulingInputDifferences {
	return []*orb.SchedulingInputDifferences{
		SchedulingInputDifferences(ctx, envClient),
	}
}

func StateNode() *state.StateNode {
	stateNode := state.NewNode()
	stateNode.Node = Node()
	stateNode.NodeClaim = NodeClaim()
	return stateNode
}

func StateNodeWithPods(stateNode *state.StateNode) *orb.StateNodeWithPods {
	return &orb.StateNodeWithPods{
		Node:      stateNode.Node,
		NodeClaim: stateNode.NodeClaim,
		Pods:      []*corev1.Pod{Pod()},
	}
}

func BindingsMap() orb.BindingsMap {
	return orb.BindingsMap{}
}

// Using test functions / metadata as appropriate, fill in...
func InstanceType() *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name:         RandomName(),
		Requirements: scheduling.Requirements{},
		Offerings:    []cloudprovider.Offering{},
		Capacity:     map[corev1.ResourceName]resource.Quantity{},
		Overhead:     &cloudprovider.InstanceTypeOverhead{},
	}
}

func SchedulingInput(ctx context.Context, envClient client.Client) *orb.SchedulingInput {
	PendingPods := []*corev1.Pod{Pod()}
	StateNode := StateNode()
	StateNodes := []*state.StateNode{StateNode}
	// set statenodes...
	Bindings := BindingsMap()
	InstanceTypes := map[string][]*cloudprovider.InstanceType{"default": {InstanceType()}}
	Topology := &scheduler.Topology{}
	DaemonSetPods := []*corev1.Pod{Pod()}

	schedulingInput := orb.NewSchedulingInput(ctx, envClient, time.Time{}, PendingPods, StateNodes, Bindings, InstanceTypes, Topology, DaemonSetPods)

	schedulingInput.StateNodesWithPods = []*orb.StateNodeWithPods{StateNodeWithPods(StateNode)}

	schedulingInput.PVList = &corev1.PersistentVolumeList{Items: []corev1.PersistentVolume{*PersistentVolume()}}
	schedulingInput.PVCList = &corev1.PersistentVolumeClaimList{Items: []corev1.PersistentVolumeClaim{*PersistentVolumeClaim()}}
	schedulingInput.ScheduledPodList = &corev1.PodList{Items: []corev1.Pod{*Pod()}}

	return &schedulingInput
}
