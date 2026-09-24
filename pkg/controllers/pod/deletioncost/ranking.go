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

// RankNodes partitions Karpenter-managed nodes into four disruption tiers and
// returns three ordered slices whose position implies the pod-deletion-cost
// annotation to apply.
//
//   - groupA (Group A): going-away nodes. Every entry maps to math.MinInt32.
//   - groupBC (Groups B + C): drifted first, then normal, ordered by
//     SavingsRatio DESC. Position within the slice yields the rank via
//     RankForBC(i, len(groupBC)); most-negative rank at index 0.
//   - groupD (Group D): cleanup-only nodes; annotations get cleared.
func RankNodes(ctx context.Context, kubeClient client.Client, clk clock.Clock, nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType) (groupA, groupBC, groupD []*state.StateNode, err error) {
	if len(nodes) == 0 {
		return nil, nil, nil, nil
	}

	// Cluster-wide PDB list. ValidatePodsDisruptable reuses this per node.
	pdbs, err := pdb.NewLimits(ctx, kubeClient)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing pod disruption budgets, %w", err)
	}

	// Sort once at entry by SavingsRatio DESC. lo.GroupBy preserves the
	// iteration order per partition (proven by ranking_internal_test.go), so
	// downstream slices inherit this order without a per-partition sort.
	sortBySavingsRatio(ctx, kubeClient, nodes, nodePoolToInstanceTypesMap)

	groups := lo.GroupBy(nodes, func(n *state.StateNode) nodePartition {
		return classifyNode(ctx, kubeClient, clk, n, nodePoolMap, nodePoolToInstanceTypesMap, pdbs)
	})
	disruptedBlocked := groups[partitionDisrupted]
	drifted := groups[partitionDrifted]
	normal := groups[partitionNormal]
	cleanupOnly := groups[partitionCleanupOnly]

	// Per-NodePool budget: B/C overflow lands in D.
	numNodes, disrupting := disruption.NodePoolStatsFromNodes(nodes)
	driftBudget := disruption.NodePoolBudgetMap(ctx, clk, nodePoolMap, numNodes, disrupting, v1.DisruptionReasonDrifted)
	consolidationBudget := disruption.NodePoolBudgetMap(ctx, clk, nodePoolMap, numNodes, disrupting, v1.DisruptionReasonUnderutilized)
	var driftOverflow, normalOverflow []*state.StateNode
	drifted, driftOverflow = applyPerNodePoolBudget(drifted, driftBudget)
	normal, normalOverflow = applyPerNodePoolBudget(normal, consolidationBudget)
	cleanupOnly = append(cleanupOnly, driftOverflow...)
	cleanupOnly = append(cleanupOnly, normalOverflow...)

	// Concatenate drifted then normal so index 0 of groupBC carries the
	// most-negative rank.
	groupBC = append(drifted, normal...)

	log.FromContext(ctx).V(1).WithValues(
		"totalNodes", len(disruptedBlocked)+len(groupBC)+len(cleanupOnly),
		"disruptedNodes", len(disruptedBlocked),
		"rankedNodes", len(groupBC),
		"cleanupOnlyNodes", len(cleanupOnly),
	).Info("completed node ranking")
	return disruptedBlocked, groupBC, cleanupOnly, nil
}

// RankForBC returns the pod-deletion-cost annotation value for a node at
// index i within a Groups B+C slice of length n. Rank is negative and
// contiguous: -n for index 0, -1 for the last index. Ascending rank means
// the ReplicaSet controller evicts the top of the slice first.
func RankForBC(i, n int) int {
	return -n + i
}

// applyPerNodePoolBudget is order-sensitive: nodes must be pre-sorted by
// cross-pool SavingsRatio DESC (see sortBySavingsRatio) so the sequential
// rank assignment places the highest-SavingsRatio node at the most-negative
// rank regardless of pool identity. Do not rewrite with lo.GroupBy: Go map
// iteration is non-deterministic and would randomize cross-pool priority.
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

// classifyNode routes a node to one of the four tiers. Cache-read failures
// inside ValidatePodsDisruptable route silently to Group D; WaitForCacheSync
// gates reconciles at manager startup, so cache-not-synced does not occur in
// steady state.
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
	if hasNonRSOwnedPods(pods) || isInstanceTypeUnresolvable(node, nodePoolToInstanceTypesMap) {
		return partitionCleanupOnly
	}
	// Check drift before consolidation-disabled so drifted nodes in a
	// ConsolidateAfter=nil pool still land in Group B rather than Group D.
	// The static-nodepool gate matches drift.ShouldDisrupt so we don't rank
	// nodes the drift controller will never act on.
	if isDrifted(node, nodePoolMap) {
		return partitionDrifted
	}
	if isConsolidationDisabled(node, nodePoolMap) {
		return partitionCleanupOnly
	}
	return partitionNormal
}

// isInstanceTypeUnresolvable reports whether disruption.NewCandidate would
// reject the node because its NodePool has no resolvable entry in the
// instance-type map. Routing them to Group D keeps PDC in lockstep with
// consolidation, which excludes such nodes entirely.
//
// A nil map means "unknown, skip the filter" so direct-helper tests that do
// not wire cloudProvider through still classify; empty labels get the same
// treatment.
func isInstanceTypeUnresolvable(node *state.StateNode, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType) bool {
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

// isDrifted mirrors drift.ShouldDisrupt so PDC and the drift controller agree
// on drifted candidates. Static-pool nodes are excluded: StaticDrift acts on
// them separately.
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

// hasNonRSOwnedPods reports whether the node hosts a pod that pins it:
// StatefulSet ordinals hold PVs, and bare pods can't be recreated.
func hasNonRSOwnedPods(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if pod.Namespace == "kube-system" {
			continue
		}
		if len(pod.OwnerReferences) == 0 {
			return true
		}
		for i := range pod.OwnerReferences {
			ownerKind := pod.OwnerReferences[i].Kind
			if ownerKind != "ReplicaSet" && ownerKind != "Job" && ownerKind != "DaemonSet" {
				return true
			}
		}
	}
	return false
}

// sortBySavingsRatio orders nodes by disruptionutils.SavingsRatio DESC with a
// node-name tie-break for determinism. Mirrors
// disruption.consolidation.sortCandidates so PDC and consolidation agree on
// which node to prefer.
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
			// Transient informer read miss: treat as zero-reschedulable so
			// the base-cost floor drives the ratio. The next reconcile picks
			// up the true pod list.
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
