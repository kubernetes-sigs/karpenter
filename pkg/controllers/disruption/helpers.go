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

package disruption

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/samber/lo"

	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	operatorlogging "sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

var errCandidateDeleting = fmt.Errorf("candidate is deleting")

//nolint:gocyclo
func SimulateScheduling(ctx context.Context, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner,
	candidates ...*Candidate,
) (pscheduling.Results, error) {
	candidateNames := sets.NewString(lo.Map(candidates, func(t *Candidate, i int) string { return t.Name() })...)
	nodes := cluster.Nodes()
	deletingNodes := nodes.Deleting()
	stateNodes := lo.Filter(nodes.Active(), func(n *state.StateNode, _ int) bool {
		return !candidateNames.Has(n.Name())
	})

	// We do one final check to ensure that the node that we are attempting to consolidate isn't
	// already handled for deletion by some other controller. This could happen if the node was markedForDeletion
	// between returning the candidates and getting the stateNodes above
	if _, ok := lo.Find(deletingNodes, func(n *state.StateNode) bool {
		return candidateNames.Has(n.Name())
	}); ok {
		return pscheduling.Results{}, errCandidateDeleting
	}

	// We get the pods that are on nodes that are deleting
	deletingNodePods, err := deletingNodes.ReschedulablePods(ctx, kubeClient)
	if err != nil {
		return pscheduling.Results{}, fmt.Errorf("failed to get pods from deleting nodes, %w", err)
	}
	// start by getting all pending pods
	pods, err := provisioner.GetPendingPods(ctx)
	if err != nil {
		return pscheduling.Results{}, fmt.Errorf("determining pending pods, %w", err)
	}
	for _, n := range candidates {
		pods = append(pods, n.reschedulablePods...)
	}
	pods = append(pods, deletingNodePods...)
	scheduler, err := provisioner.NewScheduler(log.IntoContext(ctx, operatorlogging.NopLogger), pods, stateNodes)
	if err != nil {
		return pscheduling.Results{}, fmt.Errorf("creating scheduler, %w", err)
	}

	deletingNodePodKeys := lo.SliceToMap(deletingNodePods, func(p *v1.Pod) (client.ObjectKey, interface{}) {
		return client.ObjectKeyFromObject(p), nil
	})

	results := scheduler.Solve(log.IntoContext(ctx, operatorlogging.NopLogger), pods).TruncateInstanceTypes(pscheduling.MaxInstanceTypes)
	for _, n := range results.ExistingNodes {
		// We consider existing nodes for scheduling. When these nodes are unmanaged, their taint logic should
		// tell us if we can schedule to them or not; however, if these nodes are managed, we will still schedule to them
		// even if they are still in the middle of their initialization loop. In the case of disruption, we don't want
		// to proceed disrupting if our scheduling decision relies on nodes that haven't entered a terminal state.
		if !n.Initialized() {
			for _, p := range n.Pods {
				// Only add a pod scheduling error if it isn't on an already deleting node.
				// If the pod is on a deleting node, we assume one of two things has already happened:
				// 1. The node was manually terminated, at which the provisioning controller has scheduled or is scheduling a node
				//    for the pod.
				// 2. The node was chosen for a previous disruption command, we assume that the uninitialized node will come up
				//    for this command, and we assume it will be successful. If it is not successful, the node will become
				//    not terminating, and we will no longer need to consider these pods.
				if _, ok := deletingNodePodKeys[client.ObjectKeyFromObject(p)]; !ok {
					results.PodErrors[p] = NewUninitializedNodeError(n)
				}
			}
		}
	}
	return results, nil
}

// UninitializedNodeError tracks a special pod error for disruption where pods schedule to a node
// that hasn't been initialized yet, meaning that we can't be confident to make a disruption decision based off of it
type UninitializedNodeError struct {
	*pscheduling.ExistingNode
}

func NewUninitializedNodeError(node *pscheduling.ExistingNode) *UninitializedNodeError {
	return &UninitializedNodeError{ExistingNode: node}
}

func (u *UninitializedNodeError) Error() string {
	var info []string
	if u.NodeClaim != nil {
		info = append(info, fmt.Sprintf("nodeclaim/%s", u.NodeClaim.Name))
	}
	if u.Node != nil {
		info = append(info, fmt.Sprintf("node/%s", u.Node.Name))
	}
	return fmt.Sprintf("would schedule against uninitialized %s", strings.Join(info, ", "))
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
			log.FromContext(ctx).Error(err, "parsing %s=%s from pod %s, %s",
				v1.PodDeletionCost, podDeletionCostStr, client.ObjectKeyFromObject(p))
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

// filterByPrice returns the instanceTypes that are lower priced than the current candidate.
// The minValues requirement is checked again after filterByPrice as it may result in more constrained InstanceTypeOptions for a NodeClaim.
// If the result of the filtering means that minValues can't be satisfied, we return an error.
func filterByPrice(options []*cloudprovider.InstanceType, reqs scheduling.Requirements, price float64) ([]*cloudprovider.InstanceType, error) {
	var res cloudprovider.InstanceTypes
	for _, it := range options {
		launchPrice := worstLaunchPrice(it.Offerings.Available(), reqs)
		if launchPrice < price {
			res = append(res, it)
		}
	}
	if reqs.HasMinValues() {
		// We would have already filtered the invalid NodeClaim not meeting the minimum requirements in simulated scheduling results.
		// Here the instanceTypeOptions changed again based on the price and requires re-validation.
		if _, err := res.SatisfiesMinValues(reqs); err != nil {
			return nil, fmt.Errorf("validating minValues, %w", err)
		}
	}
	return res, nil
}

func disruptionCost(ctx context.Context, pods []*v1.Pod) float64 {
	cost := 0.0
	for _, p := range pods {
		cost += GetPodEvictionCost(ctx, p)
	}
	return cost
}

// GetCandidates returns nodes that appear to be currently deprovisionable based off of their nodePool
func GetCandidates(ctx context.Context, cluster *state.Cluster, kubeClient client.Client, recorder events.Recorder, clk clock.Clock,
	cloudProvider cloudprovider.CloudProvider, shouldDeprovision CandidateFilter, queue *orchestration.Queue,
) ([]*Candidate, error) {
	nodePoolMap, nodePoolToInstanceTypesMap, err := BuildNodePoolMap(ctx, kubeClient, cloudProvider)
	if err != nil {
		return nil, err
	}
	pdbs, err := NewPDBLimits(ctx, clk, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}
	candidates := lo.FilterMap(cluster.Nodes(), func(n *state.StateNode, _ int) (*Candidate, bool) {
		cn, e := NewCandidate(ctx, kubeClient, recorder, clk, n, pdbs, nodePoolMap, nodePoolToInstanceTypesMap, queue)
		return cn, e == nil
	})
	// Filter only the valid candidates that we should disrupt
	return lo.Filter(candidates, func(c *Candidate, _ int) bool { return shouldDeprovision(ctx, c) }), nil
}

// BuildDisruptionBudgets will return a map for nodePoolName -> numAllowedDisruptions and an error
func BuildDisruptionBudgets(ctx context.Context, cluster *state.Cluster, clk clock.Clock, kubeClient client.Client, recorder events.Recorder) (map[string]int, error) {
	nodePoolList := &v1beta1.NodePoolList{}
	if err := kubeClient.List(ctx, nodePoolList); err != nil {
		return nil, fmt.Errorf("listing node pools, %w", err)
	}
	numNodes := map[string]int{}
	deleting := map[string]int{}
	disruptionBudgetMapping := map[string]int{}
	// We need to get all the nodes in the cluster
	// Get each current active number of nodes per nodePool
	// Get the max disruptions for each nodePool
	// Get the number of deleting nodes for each of those nodePools
	// Find the difference to know how much left we can disrupt
	nodes := cluster.Nodes()
	for _, node := range nodes {
		// We only consider nodes that we own and are initialized towards the total.
		// If a node is launched/registered, but not initialized, pods aren't scheduled
		// to the node, and these are treated as unhealthy until they're cleaned up.
		// This prevents odd roundup cases with percentages where replacement nodes that
		// aren't initialized could be counted towards the total, resulting in more disruptions
		// to active nodes than desired, where Karpenter should wait for these nodes to be
		// healthy before continuing.
		if !node.Managed() || !node.Initialized() {
			continue
		}
		nodePool := node.Labels()[v1beta1.NodePoolLabelKey]
		// If the node satisfies one of the following, we subtract it from the allowed disruptions.
		// 1. Has a NotReady conditiion
		// 2. Is marked as deleting
		if cond := nodeutils.GetCondition(node.Node, v1.NodeReady); cond.Status != v1.ConditionTrue || node.MarkedForDeletion() {
			deleting[nodePool]++
		}
		numNodes[nodePool]++
	}

	for i := range nodePoolList.Items {
		nodePool := nodePoolList.Items[i]
		disruptions := nodePool.MustGetAllowedDisruptions(ctx, clk, numNodes[nodePool.Name])
		// Subtract the allowed number of disruptions from the number of already deleting nodes.
		// Floor the value since the number of deleting nodes can exceed the number of allowed disruptions.
		// Allowing this value to be negative breaks assumptions in the code used to calculate how
		// many nodes can be disrupted.
		allowedDisruptions := lo.Clamp(disruptions-deleting[nodePool.Name], 0, math.MaxInt32)
		disruptionBudgetMapping[nodePool.Name] = allowedDisruptions
		// If the nodepool is fully blocked, emit an event
		if allowedDisruptions == 0 {
			recorder.Publish(disruptionevents.NodePoolBlocked(lo.ToPtr(nodePool)))
		}
		BudgetsAllowedDisruptionsGauge.With(map[string]string{
			metrics.NodePoolLabel: nodePool.Name,
		}).Set(float64(allowedDisruptions))
	}
	return disruptionBudgetMapping, nil
}

// BuildNodePoolMap builds a provName -> nodePool map and a provName -> instanceName -> instance type map
func BuildNodePoolMap(ctx context.Context, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) (map[string]*v1beta1.NodePool, map[string]map[string]*cloudprovider.InstanceType, error) {
	nodePoolMap := map[string]*v1beta1.NodePool{}
	nodePoolList := &v1beta1.NodePoolList{}
	if err := kubeClient.List(ctx, nodePoolList); err != nil {
		return nil, nil, fmt.Errorf("listing node pools, %w", err)
	}
	nodePoolToInstanceTypesMap := map[string]map[string]*cloudprovider.InstanceType{}
	for i := range nodePoolList.Items {
		np := &nodePoolList.Items[i]
		nodePoolMap[np.Name] = np

		nodePoolInstanceTypes, err := cloudProvider.GetInstanceTypes(ctx, np)
		if err != nil {
			// don't error out on building the node pool, we just won't be able to handle any nodes that
			// were created by it
			log.FromContext(ctx).Error(err, "listing instance types for %s, %s", np.Name)
			continue
		}
		if len(nodePoolInstanceTypes) == 0 {
			continue
		}
		nodePoolToInstanceTypesMap[np.Name] = map[string]*cloudprovider.InstanceType{}
		for _, it := range nodePoolInstanceTypes {
			nodePoolToInstanceTypesMap[np.Name][it.Name] = it
		}
	}
	return nodePoolMap, nodePoolToInstanceTypesMap, nil
}

// mapCandidates maps the list of proposed candidates with the current state
func mapCandidates(proposed, current []*Candidate) []*Candidate {
	proposedNames := sets.NewString(lo.Map(proposed, func(c *Candidate, i int) string { return c.Name() })...)
	return lo.Filter(current, func(c *Candidate, _ int) bool {
		return proposedNames.Has(c.Name())
	})
}

// worstLaunchPrice gets the worst-case launch price from the offerings that are offered
// on an instance type. If the instance type has a spot offering available, then it uses the spot offering
// to get the launch price; else, it uses the on-demand launch price
func worstLaunchPrice(ofs []cloudprovider.Offering, reqs scheduling.Requirements) float64 {
	// We prefer to launch spot offerings, so we will get the worst price based on the node requirements
	if reqs.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeSpot) {
		spotOfferings := lo.Filter(ofs, func(of cloudprovider.Offering, _ int) bool {
			return of.CapacityType == v1beta1.CapacityTypeSpot && reqs.Get(v1.LabelTopologyZone).Has(of.Zone)
		})
		if len(spotOfferings) > 0 {
			return lo.MaxBy(spotOfferings, func(of1, of2 cloudprovider.Offering) bool {
				return of1.Price > of2.Price
			}).Price
		}
	}
	if reqs.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeOnDemand) {
		onDemandOfferings := lo.Filter(ofs, func(of cloudprovider.Offering, _ int) bool {
			return of.CapacityType == v1beta1.CapacityTypeOnDemand && reqs.Get(v1.LabelTopologyZone).Has(of.Zone)
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
