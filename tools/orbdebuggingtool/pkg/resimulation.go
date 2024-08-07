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

package pkg

import (
	"context"

	"k8s.io/utils/clock"
	_ "knative.dev/pkg/system/testing"
	"sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/orb"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// Resimulate a scheduling run using the scheduling input reconstructed from the logs.
func Resimulate(reconstructedSchedulingInput *orb.SchedulingInput, nodePools []*v1.NodePool) (scheduler.Results, error) {
	// Reconstruct the dynamic fields from the logged inputs
	stateNodes := getStateNodesFromSchedulingInput(reconstructedSchedulingInput)
	instanceTypes := getInstanceTypesFromSchedulingInput(reconstructedSchedulingInput)
	pendingPods := reconstructedSchedulingInput.PendingPods
	topology := reconstructedSchedulingInput.Topology
	daemonSetPods := reconstructedSchedulingInput.DaemonSetPods
	pvList := reconstructedSchedulingInput.PVList
	pvcList := reconstructedSchedulingInput.PVCList
	allPods := reconstructedSchedulingInput.ScheduledPodList

	// Set-up context, client, and cluster, and reconstruct the cluster state
	ctx := context.Background()
	dataClient := New(pvList, pvcList)
	cluster := state.NewCluster(clock.RealClock{}, dataClient)
	cluster.ReconstructCluster(ctx, reconstructedSchedulingInput.Bindings, stateNodes, daemonSetPods, allPods)

	// Run the scheduling simulation
	s := scheduler.NewScheduler(dataClient, nodePools, cluster, cluster.Nodes(), topology, instanceTypes, daemonSetPods, nil)
	return s.Solve(ctx, pendingPods), nil
}

// Extracts the statenodes from the scheduling input.
func getStateNodesFromSchedulingInput(schedulingInput *orb.SchedulingInput) []*state.StateNode {
	stateNodes := []*state.StateNode{}
	for _, stateNodeWithPods := range schedulingInput.StateNodesWithPods {
		stateNodes = append(stateNodes, &state.StateNode{
			Node:      stateNodeWithPods.Node,
			NodeClaim: stateNodeWithPods.NodeClaim,
		})
	}
	// change
	return stateNodes
}

// Extract and reassociate nodepools to the instance types from the scheduling input.
func getInstanceTypesFromSchedulingInput(schedulingInput *orb.SchedulingInput) map[string][]*cloudprovider.InstanceType {
	instanceTypes := map[string][]*cloudprovider.InstanceType{}
	allInstanceTypesMap := orb.CreateMapFromSlice(schedulingInput.AllInstanceTypes, orb.GetInstanceTypeKey)
	for nodepoolName, instanceTypeNames := range schedulingInput.NodePoolInstanceTypes {
		instanceTypes[nodepoolName] = []*cloudprovider.InstanceType{}
		for _, instanceTypeName := range instanceTypeNames {
			instanceTypes[nodepoolName] = append(instanceTypes[nodepoolName], allInstanceTypesMap[instanceTypeName])
		}
	}
	return instanceTypes
}
