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
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	pscheduling "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	nodeutils "github.com/aws/karpenter-core/pkg/utils/node"
	"github.com/aws/karpenter-core/pkg/utils/pod"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//nolint:gocyclo
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
	deletingNodePods, err := nodeutils.GetNodePods(ctx, kubeClient, lo.Map(markedForDeletionNodes, func(n *state.Node, _ int) *v1.Node { return n.Node })...)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get pods from deleting nodes, %w", err)
	}

	// start by getting all pending pods
	pods, err := provisioner.GetPendingPods(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("determining pending pods, %w", err)
	}

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

	// check if the scheduling relied on an existing node that isn't ready yet, if so we fail
	// to schedule since we want to assume that we can delete a node and its pods will immediately
	// move to an existing node which won't occur if that node isn't ready.
	for _, n := range ifn {
		if n.Node.Labels[v1alpha5.LabelNodeInitialized] != "true" {
			return nil, false, nil
		}
	}
	return newNodes, podsScheduled == len(pods), nil
}

// instanceTypesAreSubset returns true if the lhs slice of instance types are a subset of the rhs.
func instanceTypesAreSubset(lhs []*cloudprovider.InstanceType, rhs []*cloudprovider.InstanceType) bool {
	rhsNames := sets.NewString(lo.Map(rhs, func(t *cloudprovider.InstanceType, i int) string { return t.Name })...)
	lhsNames := sets.NewString(lo.Map(lhs, func(t *cloudprovider.InstanceType, i int) string { return t.Name })...)
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

func filterByPrice(options []*cloudprovider.InstanceType, price float64) []*cloudprovider.InstanceType {
	var result []*cloudprovider.InstanceType
	for _, it := range options {
		// Only consider offerings that are cheaper than the current node's price
		it.Offerings = lo.Filter(it.Offerings, func(o cloudprovider.Offering, _ int) bool {
			return o.Price < price
		})
		if len(it.Offerings) > 0 {
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

type CandidateFilter func(context.Context, *state.Node, *v1alpha5.Provisioner, []*v1.Pod) bool

// candidateNodes returns nodes that appear to be currently deprovisionable based off of their provisioner
// nolint:gocyclo
func candidateNodes(ctx context.Context, cluster *state.Cluster, kubeClient client.Client, clk clock.Clock, cloudProvider cloudprovider.CloudProvider, shouldDeprovision CandidateFilter) ([]CandidateNode, error) {
	provisioners, instanceTypesByProvisioner, err := buildProvisionerMap(ctx, kubeClient, cloudProvider)
	if err != nil {
		return nil, err
	}

	var nodes []CandidateNode
	cluster.ForEachNode(func(n *state.Node) bool {
		var provisioner *v1alpha5.Provisioner
		var instanceTypeMap map[string]*cloudprovider.InstanceType
		if provName, ok := n.Node.Labels[v1alpha5.ProvisionerNameLabelKey]; ok {
			provisioner = provisioners[provName]
			instanceTypeMap = instanceTypesByProvisioner[provName]
		}
		// skip any nodes that are already marked for deletion and being handled
		if n.MarkedForDeletion {
			return true
		}
		// skip any nodes where we can't determine the provisioner
		if provisioner == nil || instanceTypeMap == nil {
			return true
		}

		instanceType, ok := instanceTypeMap[n.Node.Labels[v1.LabelInstanceTypeStable]]
		// skip any nodes that we can't determine the instance of
		if !ok {
			return true
		}

		// skip any nodes that we can't determine the capacity type or the topology zone for
		ct, ok := n.Node.Labels[v1alpha5.LabelCapacityType]
		if !ok {
			return true
		}
		az, ok := n.Node.Labels[v1.LabelTopologyZone]
		if !ok {
			return true
		}

		// Skip nodes that aren't initialized
		if n.Node.Labels[v1alpha5.LabelNodeInitialized] != "true" {
			return true
		}

		// Skip the node if it is nominated by a recent provisioning pass to be the target of a pending pod.
		if cluster.IsNodeNominated(n.Node.Name) {
			return true
		}

		pods, err := nodeutils.GetNodePods(ctx, kubeClient, n.Node)
		if err != nil {
			logging.FromContext(ctx).Errorf("Determining node pods, %s", err)
			return true
		}

		if !shouldDeprovision(ctx, n, provisioner, pods) {
			return true
		}

		cn := CandidateNode{
			Node:           n.Node,
			instanceType:   instanceType,
			capacityType:   ct,
			zone:           az,
			provisioner:    provisioner,
			pods:           pods,
			disruptionCost: disruptionCost(ctx, pods),
		}
		// lifetimeRemaining is the fraction of node lifetime remaining in the range [0.0, 1.0].  If the TTLSecondsUntilExpired
		// is non-zero, we use it to scale down the disruption costs of nodes that are going to expire.  Just after creation, the
		// disruption cost is highest and it approaches zero as the node ages towards its expiration time.
		lifetimeRemaining := calculateLifetimeRemaining(cn, clk)
		cn.disruptionCost *= lifetimeRemaining

		nodes = append(nodes, cn)
		return true
	})

	return nodes, nil
}

// buildProvisionerMap builds a provName -> provisioner map and a provName -> instanceName -> instance type map
func buildProvisionerMap(ctx context.Context, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) (map[string]*v1alpha5.Provisioner, map[string]map[string]*cloudprovider.InstanceType, error) {
	provisioners := map[string]*v1alpha5.Provisioner{}
	var provList v1alpha5.ProvisionerList
	if err := kubeClient.List(ctx, &provList); err != nil {
		return nil, nil, fmt.Errorf("listing provisioners, %w", err)
	}
	instanceTypesByProvisioner := map[string]map[string]*cloudprovider.InstanceType{}
	for i := range provList.Items {
		p := &provList.Items[i]
		provisioners[p.Name] = p

		provInstanceTypes, err := cloudProvider.GetInstanceTypes(ctx, p)
		if err != nil {
			return nil, nil, fmt.Errorf("listing instance types for %s, %w", p.Name, err)
		}
		instanceTypesByProvisioner[p.Name] = map[string]*cloudprovider.InstanceType{}
		for _, it := range provInstanceTypes {
			instanceTypesByProvisioner[p.Name][it.Name] = it
		}
	}
	return provisioners, instanceTypesByProvisioner, nil
}

// calculateLifetimeRemaining calculates the fraction of node lifetime remaining in the range [0.0, 1.0].  If the TTLSecondsUntilExpired
// is non-zero, we use it to scale down the disruption costs of nodes that are going to expire.  Just after creation, the
// disruption cost is highest and it approaches zero as the node ages towards its expiration time.
func calculateLifetimeRemaining(node CandidateNode, clock clock.Clock) float64 {
	remaining := 1.0
	if node.provisioner.Spec.TTLSecondsUntilExpired != nil {
		ageInSeconds := clock.Since(node.CreationTimestamp.Time).Seconds()
		totalLifetimeSeconds := float64(*node.provisioner.Spec.TTLSecondsUntilExpired)
		lifetimeRemainingSeconds := totalLifetimeSeconds - ageInSeconds
		remaining = clamp(0.0, lifetimeRemainingSeconds/totalLifetimeSeconds, 1.0)
	}
	return remaining
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

func canBeTerminated(node CandidateNode, pdbs *PDBLimits) bool {
	if !node.DeletionTimestamp.IsZero() {
		return false
	}
	if _, ok := pdbs.CanEvictPods(node.pods); !ok {
		return false
	}

	if _, ok := PodsPreventEviction(node.pods); ok {
		return false
	}
	return true
}

// PodsPreventEviction returns true if there are pods that would prevent eviction
func PodsPreventEviction(pods []*v1.Pod) (string, bool) {
	for _, p := range pods {
		// don't care about pods that are finishing, finished or owned by the node
		if pod.IsTerminating(p) || pod.IsTerminal(p) || pod.IsOwnedByNode(p) {
			continue
		}

		if pod.HasDoNotEvict(p) {
			return fmt.Sprintf("pod %s/%s has do not evict annotation", p.Namespace, p.Name), true
		}
	}
	return "", false
}
