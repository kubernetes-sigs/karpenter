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
//   - Group A: Disrupted+PDB-blocked or non-RS-owned-pod nodes, get math.MinInt32 (do not consume budget).
//   - Group B: Drifted nodes, sequential ranks deleted first.
//   - Group C: Normal nodes, sequential ranks deleted second.
//   - Group D: Do-not-disrupt nodes, ranked at the top so the controller clears their annotations.
//
// Per-NodePool disruption budgets bound Groups B and C. Nodes exceeding either
// budget overflow into Group D. The nodes slice must be a pre-computed
// snapshot (typically cluster.DeepCopyNodes()); RankNodes reuses it for the
// per-NodePool count so we don't pay for a second cluster-wide deep copy.
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
	// PDBs are only consulted for nodes that already carry the disrupted
	// taint; on a steady-state cluster that's rare. Skip the cluster-wide
	// list entirely when no candidate is disrupted.
	var pdbs pdb.Limits
	if anyDisrupted(nodes) {
		pdbs, err = pdb.NewLimits(ctx, kubeClient)
		if err != nil {
			return nil, fmt.Errorf("listing pod disruption budgets, %w", err)
		}
	}

	// Sort once by pod count ascending with a deterministic name tie-break.
	// partitionNodes preserves input order, so each partition inherits this
	// ordering — one sort instead of four.
	sortByPodCount(nodes, nodePods)

	disruptedBlocked, drifted, normal, doNotDisrupt := partitionNodes(ctx, clk, nodes, nodePoolMap, nodePods, pdbs)

	// Apply per-NodePool disruption budget limits to Groups B and C. Nodes
	// that exceed the budget are moved to Group D. NodePoolStatsFromNodes is
	// the shared helper that disruption.BuildDisruptionBudgetMapping also
	// uses (via NodePoolStats), so the two controllers count the same nodes
	// against the same budget; the FromNodes variant reuses this caller's
	// existing snapshot instead of paying for a second deep copy.
	numNodes, disrupting := disruption.NodePoolStatsFromNodes(nodes)
	driftBudget := buildBudgetForReason(ctx, nodePoolMap, numNodes, disrupting, clk, v1.DisruptionReasonDrifted)
	consolidationBudget := buildBudgetForReason(ctx, nodePoolMap, numNodes, disrupting, clk, v1.DisruptionReasonUnderutilized)
	var driftOverflow, normalOverflow []*state.StateNode
	drifted, driftOverflow = applyPerNodePoolBudget(drifted, driftBudget)
	normal, normalOverflow = applyPerNodePoolBudget(normal, consolidationBudget)
	doNotDisrupt = append(doNotDisrupt, driftOverflow...)
	doNotDisrupt = append(doNotDisrupt, normalOverflow...)

	// Group A nodes get math.MinInt32 and do not consume the contiguous rank
	// space below zero. Groups B and C receive sequential ranks starting at
	// -(B+C) so that drift and normal sort first under PodDeletionCost-
	// ascending semantics; the range spans -(B+C) up to -1 with no gap.
	// Group D nodes do NOT consume rank space because their annotations are
	// cleared, not written — including them in the rank walk would leave
	// visible gaps in the annotated range (e.g. 10 normal + 10 doNotDisrupt
	// would produce annotations at -20..-11 with nothing at -10..-1).
	remaining := len(drifted) + len(normal)
	currentRank := -remaining
	result := make([]NodeRank, 0, len(nodes))
	for _, node := range disruptedBlocked {
		// Group A: math.MinInt32 sentinel. We do NOT use a sequential rank
		// because every Group A node is "delete first, no questions asked";
		// distinguishing among them by pod count would imply a preference
		// the kube-scheduler shouldn't be encoding.
		// MinInt32 means max delete-priority to the RS controller (int32
		// min = int32 max-priority; the value can't go any lower).
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
		// Rank is unused for Group D — UpdatePodDeletionCosts sees
		// HasDoNotDisrupt=true and clears the annotation rather than reading
		// Rank. Leave Rank at its zero value.
		result = append(result, NodeRank{Node: node, HasDoNotDisrupt: true, Pods: nodePods[node.Name()]})
	}

	nodesRanked.Set(float64(len(result)), noLabels)
	log.FromContext(ctx).V(1).WithValues(
		"totalNodes", len(result),
		"disruptedBlockedNodes", len(disruptedBlocked),
		"driftedNodes", len(drifted),
		"normalNodes", len(normal),
		"doNotDisruptNodes", len(doNotDisrupt),
	).Info("completed node ranking")
	return result, nil
}

// applyPerNodePoolBudget walks nodes in input order and admits each into the
// bounded slice while its NodePool's remaining budget is positive; the rest
// overflow. Caller-decided what to do with the overflow (the deletion-cost
// controller routes it to Group D).
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

// fetchNodePods gathers the pod list for each candidate node into a map keyed
// by node name, so downstream helpers don't repeat the API call.
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

// partitionNodes splits nodes into four tiers. Order matters: a node that
// matches any Group A predicate takes precedence over a do-not-disrupt signal
// because Group A nodes are already on the disruption path; once Karpenter
// has tainted them, Group A is the right cohort regardless of pod
// annotations. Within the remaining nodes, do-not-disrupt and
// consolidation-disabled route to Group D, drifted to Group B, everything
// else to Group C.
//
// RFC §"Partitioning":
//   - Group A: any of (disrupted (karpenter.sh/disrupted taint) OR
//     PDB-blocked OR hosts a non-RS-owned pod). Each of the three predicates
//     reflects a "delete this node first" signal: disrupted means Karpenter
//     is already committed; PDB-blocked means consolidation is already
//     stuck; non-RS-owned means the pod has no controller to recreate it,
//     so disrupting that node is more painful than disrupting a fresh node.
//   - Group B: drifted (NodeClaim ConditionTypeDrifted=True), not in A.
//   - Group C: normal — consolidation candidates, not in A/B/D.
//   - Group D: node-level do-not-disrupt annotation, do-not-disrupt pods, or
//     NodePool with consolidation disabled (ConsolidateAfter=Never).
func partitionNodes(ctx context.Context, clk clock.Clock, nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool, nodePods map[string][]*corev1.Pod, pdbs pdb.Limits) (disruptedBlocked, drifted, normal, doNotDisrupt []*state.StateNode) {
	for _, node := range nodes {
		pods := nodePods[node.Name()]
		// Group A first — any of the three Group A signals routes here
		// regardless of do-not-disrupt signals on the node itself or its
		// pods. RFC §"Group A" calls for OR semantics across all three
		// predicates: A || B || C, not (A && B) || C.
		if isDisrupted(node) || hasPDBBlockedPods(ctx, clk, pods, pdbs) || hasNonRSOwnedPods(pods) {
			disruptedBlocked = append(disruptedBlocked, node)
			continue
		}
		if hasNodeDoNotDisrupt(node) {
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

// buildBudgetForReason computes the per-NodePool budget for a disruption
// reason as allowed - already-disrupting. A negative result is logged (it
// means the cluster has more disruptions in flight than the budget allows,
// which should not occur in steady-state) and clamped to 0 so we don't move
// every candidate to Group D.
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

// anyDisrupted reports whether any of the given state nodes has the
// karpenter.sh/disrupted taint. Used to skip the cluster-wide PDB list when
// no candidate is disrupted.
func anyDisrupted(nodes []*state.StateNode) bool {
	for _, node := range nodes {
		if isDisrupted(node) {
			return true
		}
	}
	return false
}

// hasNodeDoNotDisrupt returns true if the Node itself has the
// karpenter.sh/do-not-disrupt annotation set to "true".
func hasNodeDoNotDisrupt(node *state.StateNode) bool {
	annotations := node.Annotations()
	if annotations == nil {
		return false
	}
	return annotations[v1.DoNotDisruptAnnotationKey] == "true"
}

// isConsolidationDisabled returns true if the node's NodePool has
// consolidateAfter set to Never (nil Duration), indicating consolidation is
// disabled for that pool.
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

// isDisrupted returns true if the node has the karpenter.sh/disrupted taint,
// indicating Karpenter has already committed to disrupting this node.
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

// isDrifted returns true if the node's NodeClaim has ConditionTypeDrifted=True.
func isDrifted(node *state.StateNode) bool {
	if node.NodeClaim == nil {
		return false
	}
	return node.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()
}

// hasPDBBlockedPods reports whether any of the node's pods is currently
// blocked by a matching PDB with DisruptionsAllowed=0. Delegates to
// pdb.Limits.CanEvictPods, which is the same PDB helper the disruption
// controller uses, so the two controllers agree on what "PDB-blocked" means.
// A nil pdbs argument (no PDBs listed because no candidate is disrupted)
// short-circuits to false.
func hasPDBBlockedPods(ctx context.Context, clk clock.Clock, pods []*corev1.Pod, pdbs pdb.Limits) bool {
	if len(pods) == 0 || len(pdbs) == 0 {
		return false
	}
	_, canEvict := pdbs.CanEvictPods(pods, clk, nil)
	return !canEvict
}

// hasDoNotDisruptPods returns true if any pod on the node carries the
// karpenter.sh/do-not-disrupt annotation.
func hasDoNotDisruptPods(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if pod.Annotations[v1.DoNotDisruptAnnotationKey] == "true" {
			return true
		}
	}
	return false
}

// hasNonRSOwnedPods returns true if any non-system pod on the node is owned by
// a controller other than ReplicaSet/Job — e.g. StatefulSet, DaemonSet, raw
// Pod. The RFC's Group A definition includes these because their replacement
// path is more disruptive than evicting a ReplicaSet pod (StatefulSet pods
// have ordinal identity and persistent volumes; bare pods cannot be recreated
// at all). DaemonSet pods are excluded because they're tied to the node and
// will be replaced anyway when the node is replaced — they don't make the
// node "expensive to delete".
func hasNonRSOwnedPods(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if pod.Namespace == "kube-system" {
			continue
		}
		if len(pod.OwnerReferences) == 0 {
			// Bare pod — no controller will recreate it.
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

// sortByPodCount sorts nodes by pod count ascending with a deterministic name
// tie-break. Nodes whose pod-list lookup is missing from the map sort to the
// end (math.MaxInt sentinel) so a transient lookup failure doesn't skew the
// top of the ranking — the missing-lookup case should be impossible on the
// happy path because RankNodes pre-populates the map with one entry per
// candidate node, but the sentinel is correct-by-construction defense.
func sortByPodCount(nodes []*state.StateNode, nodePods map[string][]*corev1.Pod) {
	if len(nodes) <= 1 {
		return
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		ci := math.MaxInt
		if pods, ok := nodePods[nodes[i].Name()]; ok {
			ci = len(pods)
		}
		cj := math.MaxInt
		if pods, ok := nodePods[nodes[j].Name()]; ok {
			cj = len(pods)
		}
		if ci != cj {
			return ci < cj
		}
		return nodes[i].Name() < nodes[j].Name()
	})
}
