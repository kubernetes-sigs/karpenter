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
	"math"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
)

// RankNodes ranks nodes using PodCount strategy with four-tier partitioning:
//   - Group A: Nodes carrying the karpenter.sh/disrupted taint (Draining per
//     RFC #2935), get math.MinInt32. Do not consume budget.
//   - Group B: Drifted nodes, sequential ranks deleted first.
//   - Group C: Normal nodes, sequential ranks deleted second.
//   - Group D: Not-disruptable nodes (do-not-disrupt annotation on node or
//     pod, consolidation disabled, PDB-blocked pods, or non-RS-owned pods).
//     Annotations are cleared; RS controller uses default scale-down ordering.
//
// Per-NodePool disruption budgets bound Groups B and C. Nodes exceeding either
// budget overflow into Group D. nodes is a best-effort pointer slice from
// state.Cluster; RankNodes and NodePoolStatsFromNodes reuse it so we don't
// pay for two cluster iterations.
func RankNodes(ctx context.Context, kubeClient client.Client, clk clock.Clock, nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool) ([]NodeRank, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	defer metrics.Measure(rankingDurationSeconds, noLabels)()

	// Pre-fetch pods per node once so partition and sortByPodCount don't
	// repeat the API call.
	nodePods, err := fetchNodePods(ctx, kubeClient, nodes)
	if err != nil {
		return nil, fmt.Errorf("listing pods on candidate nodes, %w", err)
	}
	// PDB-blocked pods route to Group D on any candidate, tainted or not,
	// so the cluster-wide PDB list is needed on every reconcile.
	pdbs, err := pdb.NewLimits(ctx, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("listing pod disruption budgets, %w", err)
	}

	sortByPodCount(nodes, nodePods)

	disruptedBlocked, drifted, normal, doNotDisrupt := partitionNodes(clk, nodes, nodePoolMap, nodePods, pdbs)

	// Per-NodePool budget limits move overflow from B/C into D.
	// NodePoolStatsFromNodes is the shared helper disruption also uses (via
	// NodePoolStats), so the two controllers count the same nodes against
	// the same budget.
	numNodes, disrupting := disruption.NodePoolStatsFromNodes(nodes)
	driftBudget := buildBudgetForReason(ctx, nodePoolMap, numNodes, disrupting, clk, v1.DisruptionReasonDrifted)
	consolidationBudget := buildBudgetForReason(ctx, nodePoolMap, numNodes, disrupting, clk, v1.DisruptionReasonUnderutilized)
	var driftOverflow, normalOverflow []*state.StateNode
	drifted, driftOverflow = applyPerNodePoolBudget(drifted, driftBudget)
	normal, normalOverflow = applyPerNodePoolBudget(normal, consolidationBudget)
	doNotDisrupt = append(doNotDisrupt, driftOverflow...)
	doNotDisrupt = append(doNotDisrupt, normalOverflow...)

	// Groups B and C receive sequential ranks starting at -(B+C) up to -1
	// so drift and normal sort first under PodDeletionCost-ascending
	// semantics. Group D is excluded from the rank walk so no gap appears in
	// the annotated range — its pods get cleared, not annotated.
	remaining := len(drifted) + len(normal)
	currentRank := -remaining
	result := make([]NodeRank, 0, len(nodes))
	for _, node := range disruptedBlocked {
		// math.MinInt32 sentinel: max delete-priority to the RS controller.
		// Not sequentially ranked — every Group A node is "delete first,
		// no questions asked".
		result = append(result, NodeRank{Node: node, Rank: math.MinInt32, Pods: nodePods[node.Name()]})
	}
	for _, node := range drifted {
		result = append(result, NodeRank{Node: node, Rank: currentRank, Pods: nodePods[node.Name()]})
		currentRank++
	}
	for _, node := range normal {
		result = append(result, NodeRank{Node: node, Rank: currentRank, Pods: nodePods[node.Name()]})
		currentRank++
	}
	for _, node := range doNotDisrupt {
		// Rank unused for Group D — the queue sees HasDoNotDisrupt=true and
		// clears the annotation rather than reading Rank.
		result = append(result, NodeRank{Node: node, HasDoNotDisrupt: true, Pods: nodePods[node.Name()]})
	}

	log.FromContext(ctx).V(1).WithValues(
		"totalNodes", len(result),
		"disruptedBlockedNodes", len(disruptedBlocked),
		"driftedNodes", len(drifted),
		"normalNodes", len(normal),
		"doNotDisruptNodes", len(doNotDisrupt),
	).Info("completed node ranking")
	return result, nil
}

// applyPerNodePoolBudget admits each node until its NodePool's remaining
// budget is exhausted; the rest overflow. The caller decides what to do with
// the overflow (deletion-cost routes it to Group D).
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

// fetchNodePods gathers the pod list per node so partition and sortByPodCount
// don't repeat the API call.
func fetchNodePods(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) (map[string][]*corev1.Pod, error) {
	out := make(map[string][]*corev1.Pod, len(nodes))
	for _, node := range nodes {
		pods, err := node.Pods(ctx, kubeClient)
		if err != nil {
			return nil, fmt.Errorf("listing pods on node %q, %w", node.Name(), err)
		}
		out[node.Name()] = pods
	}
	return out, nil
}

// partitionNodes splits nodes into the four tiers documented on RankNodes.
// isDisrupted must run first: once the taint is applied the disruption path
// no longer re-checks do-not-disrupt (see disruption/queue.go and
// validation.go — both pre-taint only), so a tainted node stays in Group A
// regardless of any other signal.
func partitionNodes(clk clock.Clock, nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool, nodePods map[string][]*corev1.Pod, pdbs pdb.Limits) (disruptedBlocked, drifted, normal, doNotDisrupt []*state.StateNode) {
	for _, node := range nodes {
		pods := nodePods[node.Name()]
		if isDisrupted(node) {
			disruptedBlocked = append(disruptedBlocked, node)
			continue
		}
		if hasNodeDoNotDisrupt(node) || hasPDBBlockedPods(clk, pods, pdbs) || hasNonRSOwnedPods(pods) {
			doNotDisrupt = append(doNotDisrupt, node)
			continue
		}
		if isConsolidationDisabled(node, nodePoolMap) {
			doNotDisrupt = append(doNotDisrupt, node)
			continue
		}
		if hasDoNotDisruptPods(pods) {
			doNotDisrupt = append(doNotDisrupt, node)
			continue
		}
		if isDrifted(node) {
			drifted = append(drifted, node)
		} else {
			normal = append(normal, node)
		}
	}
	return disruptedBlocked, drifted, normal, doNotDisrupt
}

// buildBudgetForReason returns allowed - already-disrupting per NodePool.
// Negative results (more in-flight than allowed — should not occur in
// steady-state) are logged and clamped to 0 so we don't move every candidate
// to Group D.
func buildBudgetForReason(ctx context.Context, nodePoolMap map[string]*v1.NodePool, numNodes, disrupting map[string]int, clk clock.Clock, reason v1.DisruptionReason) map[string]int {
	budget := map[string]int{}
	for name, np := range nodePoolMap {
		allowed := np.MustGetAllowedDisruptions(clk, numNodes[name], reason)
		remaining := allowed - disrupting[name]
		if remaining < 0 {
			log.FromContext(ctx).V(1).WithValues(
				"nodePool", name,
				"reason", string(reason),
				"allowed", allowed,
				"disrupting", disrupting[name],
			).Info("disruption budget already exhausted; clamping to 0")
			remaining = 0
		}
		budget[name] = remaining
	}
	return budget
}

func hasNodeDoNotDisrupt(node *state.StateNode) bool {
	annotations := node.Annotations()
	if annotations == nil {
		return false
	}
	return annotations[v1.DoNotDisruptAnnotationKey] == "true"
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

func isDisrupted(node *state.StateNode) bool {
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

func isDrifted(node *state.StateNode) bool {
	if node.NodeClaim == nil {
		return false
	}
	return node.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()
}

// hasPDBBlockedPods reports whether any pod on the node is currently blocked
// by a matching PDB. Delegates to pdb.Limits.CanEvictPods, the same helper
// the disruption controller uses, so the two agree on what "PDB-blocked"
// means.
func hasPDBBlockedPods(clk clock.Clock, pods []*corev1.Pod, pdbs pdb.Limits) bool {
	if len(pods) == 0 || len(pdbs) == 0 {
		return false
	}
	_, canEvict := pdbs.CanEvictPods(pods, clk, nil)
	return !canEvict
}

func hasDoNotDisruptPods(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if pod.Annotations[v1.DoNotDisruptAnnotationKey] == "true" {
			return true
		}
	}
	return false
}

// hasNonRSOwnedPods reports whether any non-kube-system pod on the node has
// no controller (bare Pod) or is owned by a controller other than
// ReplicaSet/Job/DaemonSet — e.g. StatefulSet pins the node (SS ordinal +
// PVs; bare pods can't be recreated). DaemonSet pods are excluded because
// they get replaced when the node does.
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

// sortByPodCount sorts nodes by live pod count ascending with a deterministic
// name tie-break. Nodes with a missing pod-list lookup sort last (math.MaxInt
// sentinel) so a transient lookup failure doesn't skew the top of the
// ranking.
//
// DeletionTimestamp'd pods are excluded from the count so a mostly-terminating
// node ranks lighter than a same-count node with no drain in progress —
// consolidation prefers finishing what is already ending. The full pod list
// is retained on NodeRank so the queue still writes/clears annotations on
// deleting pods until they finish terminating.
func sortByPodCount(nodes []*state.StateNode, nodePods map[string][]*corev1.Pod) {
	if len(nodes) <= 1 {
		return
	}
	sort.Slice(nodes, func(i, j int) bool {
		ci := livePodCount(nodePods, nodes[i].Name())
		cj := livePodCount(nodePods, nodes[j].Name())
		if ci != cj {
			return ci < cj
		}
		return nodes[i].Name() < nodes[j].Name()
	})
}

// livePodCount returns the number of non-terminating pods on the named node.
// Missing map entries sort last via math.MaxInt; see sortByPodCount.
func livePodCount(nodePods map[string][]*corev1.Pod, nodeName string) int {
	pods, ok := nodePods[nodeName]
	if !ok {
		return math.MaxInt
	}
	count := 0
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil {
			count++
		}
	}
	return count
}
