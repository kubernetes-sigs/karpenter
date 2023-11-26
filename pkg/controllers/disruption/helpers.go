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

package disruption

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/samber/lo"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	disruptionevents "github.com/aws/karpenter-core/pkg/controllers/disruption/events"
	"github.com/aws/karpenter-core/pkg/controllers/disruption/orchestration"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	pscheduling "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/scheduling"
	nodeutils "github.com/aws/karpenter-core/pkg/utils/node"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
	"github.com/aws/karpenter-core/pkg/utils/pod"
)

func filterCandidates(ctx context.Context, kubeClient client.Client, recorder events.Recorder, nodes []*Candidate) ([]*Candidate, error) {
	pdbs, err := NewPDBLimits(ctx, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}

	// filter out nodes that can't be terminated
	nodes = lo.Filter(nodes, func(cn *Candidate, _ int) bool {
		if !cn.Node.DeletionTimestamp.IsZero() {
			recorder.Publish(disruptionevents.Blocked(cn.Node, cn.NodeClaim, "Node in the process of deletion")...)
			return false
		}
		if pdb, ok := pdbs.CanEvictPods(cn.pods); !ok {
			recorder.Publish(disruptionevents.Blocked(cn.Node, cn.NodeClaim, fmt.Sprintf("PDB %q prevents pod evictions", pdb))...)
			return false
		}
		if p, ok := hasDoNotDisruptPod(cn); ok {
			recorder.Publish(disruptionevents.Blocked(cn.Node, cn.NodeClaim, fmt.Sprintf("Pod %q has do not evict annotation", client.ObjectKeyFromObject(p)))...)
			return false
		}
		return true
	})
	return nodes, nil
}

//nolint:gocyclo
func simulateScheduling(ctx context.Context, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner,
	candidates ...*Candidate) (*pscheduling.Results, error) {
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
		return nil, errCandidateDeleting
	}

	// We get the pods that are on nodes that are deleting
	deletingNodePods, err := deletingNodes.Pods(ctx, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get pods from deleting nodes, %w", err)
	}
	// start by getting all pending pods
	pods, err := provisioner.GetPendingPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("determining pending pods, %w", err)
	}

	for _, n := range candidates {
		pods = append(pods, n.pods...)
	}
	pods = append(pods, deletingNodePods...)
	scheduler, err := provisioner.NewScheduler(ctx, pods, stateNodes, pscheduling.SchedulerOptions{
		SimulationMode: true,
	})

	if err != nil {
		return nil, fmt.Errorf("creating scheduler, %w", err)
	}

	results := scheduler.Solve(ctx, pods)
	// check if the scheduling relied on an existing node that isn't ready yet, if so we fail
	// to schedule since we want to assume that we can delete a node and its pods will immediately
	// move to an existing node which won't occur if that node isn't ready.
	for _, n := range results.ExistingNodes {
		if !n.Initialized() || nodeutils.GetCondition(n.Node, v1.NodeReady).Status != v1.ConditionTrue {
			for _, p := range n.Pods {
				results.PodErrors[p] = fmt.Errorf("would schedule against a non-initialized node %s", n.Name())
			}
		}
	}

	return results, nil
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

func filterByPrice(options []*cloudprovider.InstanceType, reqs scheduling.Requirements, price float64) []*cloudprovider.InstanceType {
	var result []*cloudprovider.InstanceType
	for _, it := range options {
		launchPrice := worstLaunchPrice(it.Offerings.Available(), reqs)
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

// GetCandidates returns nodes that appear to be currently deprovisionable based off of their nodePool
func GetCandidates(ctx context.Context, cluster *state.Cluster, kubeClient client.Client, recorder events.Recorder, clk clock.Clock,
	cloudProvider cloudprovider.CloudProvider, shouldDeprovision CandidateFilter, queue *orchestration.Queue) ([]*Candidate, error) {
	nodePoolMap, nodePoolToInstanceTypesMap, err := buildNodePoolMap(ctx, kubeClient, cloudProvider)
	if err != nil {
		return nil, err
	}
	candidates := lo.FilterMap(cluster.Nodes(), func(n *state.StateNode, _ int) (*Candidate, bool) {
		cn, e := NewCandidate(ctx, kubeClient, recorder, clk, n, nodePoolMap, nodePoolToInstanceTypesMap, queue)
		return cn, e == nil
	})
	// Filter only the valid candidates that we should disrupt
	return lo.Filter(candidates, func(c *Candidate, _ int) bool { return shouldDeprovision(ctx, c) }), nil
}

// buildNodePoolMap builds a provName -> nodePool map and a provName -> instanceName -> instance type map
func buildNodePoolMap(ctx context.Context, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) (map[string]*v1beta1.NodePool, map[string]map[string]*cloudprovider.InstanceType, error) {
	nodePoolMap := map[string]*v1beta1.NodePool{}
	nodePoolList, err := nodepoolutil.List(ctx, kubeClient)
	if err != nil {
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
			logging.FromContext(ctx).Errorf("listing instance types for %s, %s", np.Name, err)
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

func hasDoNotDisruptPod(c *Candidate) (*v1.Pod, bool) {
	return lo.Find(c.pods, func(p *v1.Pod) bool {
		if pod.IsTerminating(p) || pod.IsTerminal(p) || pod.IsOwnedByNode(p) {
			return false
		}
		return pod.HasDoNotDisrupt(p)
	})
}
