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

package deletioncost

import (
	"context"
	"fmt"
	"sort"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
)

// RankNodes returns three slices whose position implies the pod-deletion-cost
// annotation: groupA maps to math.MinInt32, groupBC is drifted then normal by
// SavingsRatio DESC with the rank from RankForBC(i, len(groupBC)), and groupD
// has its annotations cleared.
func RankNodes(ctx context.Context, kubeClient client.Client, clk clock.Clock, nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType) (groupA, groupBC, groupD []*state.StateNode, err error) {
	// PDC only annotates pods on nodes Karpenter owns. An unmanaged node with a
	// deletion timestamp satisfies StateNode.Deleted(), so without this filter it
	// reaches Group A and gets MinInt32 written on every pod, uncapped.
	nodes = lo.Filter(nodes, func(n *state.StateNode, _ int) bool { return n.Managed() })
	if len(nodes) == 0 {
		return nil, nil, nil, nil
	}

	pdbs, err := pdb.NewLimits(ctx, kubeClient)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing pod disruption budgets, %w", err)
	}

	// Sorted once here: lo.GroupBy preserves per-partition iteration order, so
	// each group inherits SavingsRatio DESC without its own sort.
	sortBySavingsRatio(ctx, kubeClient, nodes, nodePoolToInstanceTypesMap)

	groups := lo.GroupBy(nodes, func(n *state.StateNode) nodePartition {
		return classifyNode(ctx, kubeClient, clk, n, nodePoolMap, nodePoolToInstanceTypesMap, pdbs)
	})
	disruptedBlocked := groups[partitionDisrupted]
	drifted := groups[partitionDrifted]
	normal := groups[partitionNormal]
	cleanupOnly := groups[partitionCleanupOnly]

	budgetFor := func(reason v1.DisruptionReason) map[string]int {
		numNodes, disrupting := disruption.NodePoolStatsFromNodes(nodes, reason)
		return disruption.NodePoolBudgetMap(ctx, clk, nodePoolMap, numNodes, disrupting, reason)
	}
	driftBudget := budgetFor(v1.DisruptionReasonDrifted)
	consolidationBudget := budgetFor(v1.DisruptionReasonUnderutilized)
	var driftOverflow, normalOverflow []*state.StateNode
	drifted, driftOverflow = applyPerNodePoolBudget(drifted, driftBudget)
	normal, normalOverflow = applyPerNodePoolBudget(normal, consolidationBudget)
	cleanupOnly = append(cleanupOnly, driftOverflow...)
	cleanupOnly = append(cleanupOnly, normalOverflow...)

	// Drifted first so index 0 carries the most-negative rank.
	groupBC = append(drifted, normal...)

	log.FromContext(ctx).V(1).WithValues(
		"totalNodes", len(disruptedBlocked)+len(groupBC)+len(cleanupOnly),
		"disruptedNodes", len(disruptedBlocked),
		"rankedNodes", len(groupBC),
		"cleanupOnlyNodes", len(cleanupOnly),
	).Info("completed node ranking")
	return disruptedBlocked, groupBC, cleanupOnly, nil
}

// RankForBC returns -n at index 0 rising to -1 at index n-1. The ReplicaSet
// controller evicts the lowest value first, so index 0 goes first.
func RankForBC(i, n int) int {
	return -n + i
}

// applyPerNodePoolBudget requires nodes pre-sorted by cross-pool SavingsRatio
// DESC. Do not rewrite with lo.GroupBy: map iteration order would randomize
// cross-pool priority.
func applyPerNodePoolBudget(nodes []*state.StateNode, budget map[string]int) (bounded, overflow []*state.StateNode) {
	used := map[string]int{}
	for _, node := range nodes {
		poolName := node.Labels()[v1.NodePoolLabelKey]
		if used[poolName] < budget[poolName] {
			bounded = append(bounded, node)
			used[poolName]++
		} else {
			overflow = append(overflow, node)
		}
	}
	return bounded, overflow
}

type nodePartition int

const (
	partitionDisrupted nodePartition = iota
	partitionDrifted
	partitionNormal
	partitionCleanupOnly
)

func classifyNode(ctx context.Context, kubeClient client.Client, clk clock.Clock, node *state.StateNode, nodePoolMap map[string]*v1.NodePool, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType, pdbs pdb.Limits) nodePartition {
	if isGoingAway(node) {
		return partitionDisrupted
	}
	if verr := node.ValidateNodeDisruptable(clk); verr != nil {
		return partitionCleanupOnly
	}
	return classifyDisruptableNode(ctx, kubeClient, clk, node, nodePoolMap, nodePoolToInstanceTypesMap, pdbs)
}

func classifyDisruptableNode(ctx context.Context, kubeClient client.Client, clk clock.Clock, node *state.StateNode, nodePoolMap map[string]*v1.NodePool, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType, pdbs pdb.Limits) nodePartition {
	pods, verr := node.ValidatePodsDisruptable(ctx, kubeClient, pdbs, clk, nil)
	if verr != nil {
		return partitionCleanupOnly
	}
	if hasPinningPods(pods) || isUnpriceable(node, nodePoolToInstanceTypesMap) {
		return partitionCleanupOnly
	}
	// Drift is checked first so a drifted node in a ConsolidateAfter=nil pool
	// still ranks.
	if isDrifted(node, nodePoolMap) {
		return partitionDrifted
	}
	if isConsolidationDisabled(node, nodePoolMap) {
		return partitionCleanupOnly
	}
	return partitionNormal
}

// isUnpriceable reports whether the node's NodePool offers no instance type
// matching the node's instance-type label. ResolveOfferingPrice returns 0 for
// such a node. groupBC is ordered by savings ratio, price over reschedule cost,
// so every unpriceable node sits at ratio 0, ties with the rest of them, and
// breaks on name. That position says nothing about how cheap the node is to
// disrupt, so it routes to partitionCleanupOnly, where rank is unused.
//
// The early returns cover the case where nothing was priced at all: a nil map,
// or a node missing the NodePool or instance-type label. No ranking is being
// corrupted then, so the filter stands down instead of routing every node to
// cleanup.
func isUnpriceable(node *state.StateNode, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType) bool {
	if nodePoolToInstanceTypesMap == nil {
		return false
	}
	nodePoolName := node.Labels()[v1.NodePoolLabelKey]
	itName := node.Labels()[corev1.LabelInstanceTypeStable]
	if nodePoolName == "" || itName == "" {
		return false
	}
	instanceTypeMap, ok := nodePoolToInstanceTypesMap[nodePoolName]
	if !ok || instanceTypeMap == nil {
		return true
	}
	if _, ok := instanceTypeMap[itName]; !ok {
		return true
	}
	return false
}

func isConsolidationDisabled(node *state.StateNode, nodePoolMap map[string]*v1.NodePool) bool {
	nodePoolName := node.Labels()[v1.NodePoolLabelKey]
	if nodePoolName == "" {
		return false
	}
	np, ok := nodePoolMap[nodePoolName]
	if !ok {
		return false
	}
	return np.Spec.Disruption.ConsolidateAfter.Duration == nil
}

func isGoingAway(node *state.StateNode) bool {
	if node.MarkedForDeletion() {
		return true
	}
	if node.Node == nil {
		return false
	}
	for i := range node.Node.Spec.Taints {
		if node.Node.Spec.Taints[i].MatchTaint(&v1.DisruptedNoScheduleTaint) {
			return true
		}
	}
	return false
}

// isDrifted mirrors drift.ShouldDisrupt. Static-pool nodes are excluded because
// StaticDrift acts on them separately.
//
// TODO: the drift-condition read and IsStatic gate are duplicated in
// disruption/drift.go and disruption/staticdrift.go; dedupe in a follow-up.
func isDrifted(node *state.StateNode, nodePoolMap map[string]*v1.NodePool) bool {
	if node.NodeClaim == nil {
		return false
	}
	if np := nodePoolMap[node.Labels()[v1.NodePoolLabelKey]]; np != nil && nodepoolutils.IsStatic(np) {
		return false
	}
	return node.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()
}

// Controllers that replace a pod they own somewhere else in the cluster, so a
// pod under one of them does not pin its node.
var recreatingControllers = []schema.GroupVersionKind{
	{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
	{Group: "batch", Version: "v1", Kind: "Job"},
	{Group: "apps", Version: "v1", Kind: "DaemonSet"},
}

// hasPinningPods reports whether the node hosts a pod that pins it, meaning
// nothing would bring that pod back elsewhere: StatefulSet ordinals hold PVs to
// a zone, and a pod no controller claims has nothing to replace it at all.
//
// Controller reference, not any owner reference. Only the controller replaces a
// deleted pod; a non-controller owner reference is garbage-collection linkage
// and does not make a pod recreatable. That also makes "is this pod
// ReplicaSet-owned" mean the same thing here as it does at the enqueue gate,
// which reads it through the same primitive.
func hasPinningPods(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if pod.Namespace == "kube-system" {
			continue
		}
		if !podutils.IsControlledBy(pod, recreatingControllers) {
			return true
		}
	}
	return false
}

// sortBySavingsRatio mirrors disruption.consolidation.sortCandidates so PDC and
// consolidation prefer the same node.
func sortBySavingsRatio(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType) {
	if len(nodes) <= 1 {
		return
	}
	ratio := make(map[string]float64, len(nodes))
	for _, n := range nodes {
		labels := n.Labels()
		var it *cloudprovider.InstanceType
		if m := nodePoolToInstanceTypesMap[labels[v1.NodePoolLabelKey]]; m != nil {
			it = m[labels[corev1.LabelInstanceTypeStable]]
		}
		offeringPrice := disruptionutils.ResolveOfferingPrice(labels, it)
		pods, err := n.Pods(ctx, kubeClient)
		if err != nil {
			// Fall back to the base-cost floor; the next reconcile re-reads.
			log.FromContext(ctx).V(1).WithValues("node", n.Name()).Error(err, "listing pods for savings-ratio sort; using base cost")
			pods = nil
		}
		reschedulable := lo.Filter(pods, func(p *corev1.Pod, _ int) bool { return podutils.IsReschedulable(p) })
		disruptionCost := disruptionutils.ComputeRescheduleDisruptionCost(ctx, reschedulable)
		ratio[n.Name()] = disruptionutils.SavingsRatio(offeringPrice, disruptionCost)
	}
	sort.Slice(nodes, func(i, j int) bool {
		ri, rj := ratio[nodes[i].Name()], ratio[nodes[j].Name()]
		if ri != rj {
			return ri > rj
		}
		return nodes[i].Name() < nodes[j].Name()
	})
}
