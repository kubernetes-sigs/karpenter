/*
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

package deprovisioning

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	pscheduling "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/utils/node"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func simulateScheduling(ctx context.Context, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner,
	nodesToDelete ...CandidateNode) (newNodes []*pscheduling.Node, allPodsScheduled bool, err error) {
	var stateNodes []*state.Node
	var markedForDeletionNodes []*state.Node
	candidateNodeIsDeleting := false
	candidateNodeNames := sets.NewString(lo.Map(nodesToDelete, func(t CandidateNode, i int) string { return t.Name })...)
	cluster.ForEachNode(func(n *state.Node) bool {
		// not a candidate node
		if _, ok := candidateNodeNames[n.Node.Name]; !ok {
			if !n.MarkedForDeletion {
				stateNodes = append(stateNodes, n.DeepCopy())
			} else {
				markedForDeletionNodes = append(markedForDeletionNodes, n.DeepCopy())
			}
		} else if n.MarkedForDeletion {
			// candidate node and marked for deletion
			candidateNodeIsDeleting = true
		}
		return true
	})
	// We do one final check to ensure that the node that we are attempting to consolidate isn't
	// already handled for deletion by some other controller. This could happen if the node was markedForDeletion
	// between returning the candidateNodes and getting the stateNodes above
	if candidateNodeIsDeleting {
		return nil, false, errCandidateNodeDeleting
	}

	// We get the pods that are on nodes that are deleting
	deletingNodePods, err := node.GetNodePods(ctx, kubeClient, lo.Map(markedForDeletionNodes, func(n *state.Node, _ int) *v1.Node { return n.Node })...)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get pods from deleting nodes, %w", err)
	}
	var pods []*v1.Pod
	for _, n := range nodesToDelete {
		pods = append(pods, n.pods...)
	}
	pods = append(pods, deletingNodePods...)
	scheduler, err := provisioner.NewScheduler(ctx, pods, stateNodes, pscheduling.SchedulerOptions{
		SimulationMode: true,
	})

	if err != nil {
		return nil, false, fmt.Errorf("creating scheduler, %w", err)
	}

	newNodes, ifn, err := scheduler.Solve(ctx, pods)
	if err != nil {
		return nil, false, fmt.Errorf("simulating scheduling, %w", err)
	}

	podsScheduled := 0
	for _, n := range newNodes {
		podsScheduled += len(n.Pods)
	}
	for _, n := range ifn {
		podsScheduled += len(n.Pods)
	}

	return newNodes, podsScheduled == len(pods), nil
}

// instanceTypesAreSubset returns true if the lhs slice of instance types are a subset of the rhs.
func instanceTypesAreSubset(lhs []cloudprovider.InstanceType, rhs []cloudprovider.InstanceType) bool {
	rhsNames := sets.NewString(lo.Map(rhs, func(t cloudprovider.InstanceType, i int) string { return t.Name() })...)
	lhsNames := sets.NewString(lo.Map(lhs, func(t cloudprovider.InstanceType, i int) string { return t.Name() })...)
	return len(rhsNames.Intersection(lhsNames)) == len(lhsNames)
}

// GetPodEvictionCost returns the disruption cost computed for evicting the given pod.
func GetPodEvictionCost(ctx context.Context, p *v1.Pod) float64 {
	cost := 1.0
	podDeletionCostStr, ok := p.Annotations[v1.PodDeletionCost]
	if ok {
		podDeletionCost, err := strconv.ParseFloat(podDeletionCostStr, 64)
		if err != nil {
			logging.FromContext(ctx).Errorf("parsing %s=%s from pod %s, %s",
				v1.PodDeletionCost, podDeletionCostStr, client.ObjectKeyFromObject(p), err)
		} else {
			// the pod deletion disruptionCost is in [-2147483647, 2147483647]
			// the min pod disruptionCost makes one pod ~ -15 pods, and the max pod disruptionCost to ~ 17 pods.
			cost += podDeletionCost / math.Pow(2, 27.0)
		}
	}
	// the scheduling priority is in [-2147483648, 1000000000]
	if p.Spec.Priority != nil {
		cost += float64(*p.Spec.Priority) / math.Pow(2, 25)
	}

	// overall we clamp the pod cost to the range [-10.0, 10.0] with the default being 1.0
	return clamp(-10.0, cost, 10.0)
}

func filterByPrice(options []cloudprovider.InstanceType, reqs scheduling.Requirements, price float64) []cloudprovider.InstanceType {
	var result []cloudprovider.InstanceType
	for _, it := range options {
		launchPrice := worstLaunchPrice(cloudprovider.AvailableOfferings(it), reqs)
		if launchPrice < price {
			result = append(result, it)
		}
	}
	return result
}

func disruptionCost(ctx context.Context, pods []*v1.Pod) float64 {
	cost := 0.0
	for _, p := range pods {
		cost += GetPodEvictionCost(ctx, p)
	}
	return cost
}

// worstLaunchPrice gets the worst-case launch price from the offerings that are offered
// on an instance type. If the instance type has a spot offering available, then it uses the spot offering
// to get the launch price; else, it uses the on-demand launch price
func worstLaunchPrice(ofs []cloudprovider.Offering, reqs scheduling.Requirements) float64 {
	// We prefer to launch spot offerings, so we will get the worst price based on the node requirements
	if reqs.Get(v1alpha5.LabelCapacityType).Has(v1alpha5.CapacityTypeSpot) {
		spotOfferings := lo.Filter(ofs, func(of cloudprovider.Offering, _ int) bool {
			return of.CapacityType == v1alpha5.CapacityTypeSpot && reqs.Get(v1.LabelTopologyZone).Has(of.Zone)
		})
		if len(spotOfferings) > 0 {
			return lo.MaxBy(spotOfferings, func(of1, of2 cloudprovider.Offering) bool {
				return of1.Price > of2.Price
			}).Price
		}
	}
	if reqs.Get(v1alpha5.LabelCapacityType).Has(v1alpha5.CapacityTypeOnDemand) {
		onDemandOfferings := lo.Filter(ofs, func(of cloudprovider.Offering, _ int) bool {
			return of.CapacityType == v1alpha5.CapacityTypeOnDemand && reqs.Get(v1.LabelTopologyZone).Has(of.Zone)
		})
		if len(onDemandOfferings) > 0 {
			return lo.MaxBy(onDemandOfferings, func(of1, of2 cloudprovider.Offering) bool {
				return of1.Price > of2.Price
			}).Price
		}
	}
	return math.MaxFloat64
}

func clamp(min, val, max float64) float64 {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

// mapNodes maps from a list of *v1.Node to candidateNode
func mapNodes(nodes []*v1.Node, candidateNodes []CandidateNode) []CandidateNode {
	verifyNodeNames := sets.NewString(lo.Map(nodes, func(t *v1.Node, i int) string { return t.Name })...)
	var ret []CandidateNode
	for _, c := range candidateNodes {
		if verifyNodeNames.Has(c.Name) {
			ret = append(ret, c)
		}
	}
	return ret
}
