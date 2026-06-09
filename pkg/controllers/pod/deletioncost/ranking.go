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
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

// NodeRank pairs a state node with the rank value the controller plans to
// write to its pods. HasDoNotDisrupt indicates Group D, where the controller
// clears the annotation rather than writing rank.
type NodeRank struct {
	Node            *state.StateNode
	Rank            int
	HasDoNotDisrupt bool
}

// RankingEngine implements PodCount-based node ranking with four-tier
// partitioning. Dependencies are injected once via NewRankingEngine so the
// per-call API is small. Exported so tests can drive the engine directly.
type RankingEngine struct {
	kubeClient client.Client
	cluster    *state.Cluster
	clock      clock.Clock
}

// NewRankingEngine returns a RankingEngine bound to the given dependencies.
func NewRankingEngine(kubeClient client.Client, cluster *state.Cluster, clk clock.Clock) *RankingEngine {
	return &RankingEngine{kubeClient: kubeClient, cluster: cluster, clock: clk}
}

// partitionResult holds the four-way split of nodes produced by partitionNodes.
type partitionResult struct {
	disruptedBlocked []*state.StateNode
	drifted          []*state.StateNode
	normal           []*state.StateNode
	doNotDisrupt     []*state.StateNode
}

// RankNodes ranks nodes using PodCount strategy with four-tier partitioning:
//   - Group A: Disrupted + PDB-blocked nodes, get math.MinInt32 (do not consume budget).
//   - Group B: Drifted nodes, sequential ranks deleted first.
//   - Group C: Normal nodes, sequential ranks deleted second.
//   - Group D: Do-not-disrupt nodes, ranked at the top so the controller clears their annotations.
//
// Per-NodePool disruption budgets bound Groups B and C. Nodes exceeding either
// budget overflow into Group D.
func (r *RankingEngine) RankNodes(ctx context.Context, nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool) ([]NodeRank, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	defer metrics.Measure(rankingDurationSeconds, noLabels)()

	// Pre-fetch pods per node once so partitionNodes and sortByPodCount don't
	// repeat the API call.
	nodePods, err := r.fetchNodePods(ctx, nodes)
	if err != nil {
		return nil, fmt.Errorf("listing pods on candidate nodes, %w", err)
	}
	// PDBs are only consulted for nodes that already carry the disrupted
	// taint; on a steady-state cluster that's rare. Skip the cluster-wide
	// list entirely when no candidate is disrupted.
	var pdbs []parsedPDB
	if anyDisrupted(nodes) {
		pdbs, err = r.fetchPDBs(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing pod disruption budgets, %w", err)
		}
	}

	parts := r.partitionNodes(nodes, nodePoolMap, nodePods, pdbs)

	r.sortByPodCount(parts.disruptedBlocked, nodePods)
	r.sortByPodCount(parts.drifted, nodePods)
	r.sortByPodCount(parts.normal, nodePods)
	r.sortByPodCount(parts.doNotDisrupt, nodePods)

	// Apply per-NodePool disruption budget limits to Groups B and C. Nodes
	// that exceed the budget are moved to Group D.
	drifted, normal, overflow := r.applyBudgetLimits(ctx, parts.drifted, parts.normal, nodePoolMap)
	parts.drifted = drifted
	parts.normal = normal
	parts.doNotDisrupt = append(parts.doNotDisrupt, overflow...)

	// Group A nodes get math.MinInt32 and do not consume the contiguous rank
	// space below zero. The remaining groups receive sequential ranks starting
	// at -(B+C+D) so that drift and normal sort first under
	// PodDeletionCost-ascending semantics.
	remaining := len(parts.drifted) + len(parts.normal) + len(parts.doNotDisrupt)
	currentRank := -remaining
	result := make([]NodeRank, 0, len(nodes))
	for _, node := range parts.disruptedBlocked {
		result = append(result, NodeRank{Node: node, Rank: math.MinInt32})
	}
	for _, node := range parts.drifted {
		result = append(result, NodeRank{Node: node, Rank: currentRank})
		currentRank++
	}
	for _, node := range parts.normal {
		result = append(result, NodeRank{Node: node, Rank: currentRank})
		currentRank++
	}
	for _, node := range parts.doNotDisrupt {
		result = append(result, NodeRank{Node: node, Rank: currentRank, HasDoNotDisrupt: true})
		currentRank++
	}

	nodesRankedTotal.Add(float64(len(result)), noLabels)
	log.FromContext(ctx).V(1).WithValues(
		"totalNodes", len(result),
		"disruptedBlockedNodes", len(parts.disruptedBlocked),
		"driftedNodes", len(parts.drifted),
		"normalNodes", len(parts.normal),
		"doNotDisruptNodes", len(parts.doNotDisrupt),
	).Info("completed node ranking")
	return result, nil
}

// fetchNodePods gathers the pod list for each candidate node into a map keyed
// by node name, so downstream helpers don't repeat the API call.
func (r *RankingEngine) fetchNodePods(ctx context.Context, nodes []*state.StateNode) (map[string][]*corev1.Pod, error) {
	out := make(map[string][]*corev1.Pod, len(nodes))
	for _, node := range nodes {
		pods, err := node.Pods(ctx, r.kubeClient)
		if err != nil {
			return nil, fmt.Errorf("listing pods on node %q, %w", node.Name(), err)
		}
		out[node.Name()] = pods
	}
	return out, nil
}

// parsedPDB pairs a PDB with its pre-parsed selector. We pre-parse once per
// reconcile so the per-pod inner loop in hasPDBBlockedPods is allocation-free.
type parsedPDB struct {
	pdb      *policyv1.PodDisruptionBudget
	selector labels.Selector
}

// fetchPDBs lists every PDB and pre-parses its selector. Malformed selectors
// fail closed (the PDB is treated as matching every pod in its namespace) so
// the controller errs on the side of classifying nodes as PDB-blocked rather
// than ignoring potentially-blocking PDBs.
func (r *RankingEngine) fetchPDBs(ctx context.Context) ([]parsedPDB, error) {
	var list policyv1.PodDisruptionBudgetList
	if err := r.kubeClient.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]parsedPDB, 0, len(list.Items))
	for i := range list.Items {
		pdb := &list.Items[i]
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			log.FromContext(ctx).V(1).WithValues("pdb", client.ObjectKeyFromObject(pdb)).Error(err, "PDB selector failed to parse, treating as match-everything (fail-closed)")
			selector = labels.Everything()
		}
		out = append(out, parsedPDB{pdb: pdb, selector: selector})
	}
	return out, nil
}

// partitionNodes splits nodes into four tiers:
//
//	A. disruptedBlocked - karpenter.sh/disrupted taint AND at least one pod blocked by a PDB
//	B. drifted          - NodeClaim has ConditionTypeDrifted=True (and not in A)
//	C. normal           - not drifted, no do-not-disrupt pods, consolidation enabled
//	D. doNotDisrupt     - node-level do-not-disrupt annotation, do-not-disrupt pods,
//	                     or NodePool with consolidation disabled
func (r *RankingEngine) partitionNodes(nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool, nodePods map[string][]*corev1.Pod, pdbs []parsedPDB) partitionResult {
	var p partitionResult
	for _, node := range nodes {
		if hasNodeDoNotDisrupt(node) {
			p.doNotDisrupt = append(p.doNotDisrupt, node)
			continue
		}
		if isConsolidationDisabled(node, nodePoolMap) {
			p.doNotDisrupt = append(p.doNotDisrupt, node)
			continue
		}
		pods := nodePods[node.Name()]
		if hasDoNotDisruptPods(pods) {
			p.doNotDisrupt = append(p.doNotDisrupt, node)
			continue
		}
		if isDisrupted(node) && hasPDBBlockedPods(pods, pdbs) {
			p.disruptedBlocked = append(p.disruptedBlocked, node)
			continue
		}
		if isDrifted(node) {
			p.drifted = append(p.drifted, node)
		} else {
			p.normal = append(p.normal, node)
		}
	}
	return p
}

// applyBudgetLimits enforces per-NodePool disruption budgets on Groups B and C.
// Group B (drifted) nodes are bounded by the drift disruption budget and
// Group C (normal) nodes are bounded by the consolidation (Underutilized)
// budget. Nodes that exceed the budget are returned in the overflow slice so
// the caller can move them to Group D.
//
// This deliberately mirrors the approach in
// pkg/controllers/disruption/helpers.go BuildDisruptionBudgetMapping. A future
// refactor to share the helper directly is tracked as a follow-up so the
// disruption controller and the deletion-cost controller stay in lockstep on
// the budget computation.
func (r *RankingEngine) applyBudgetLimits(ctx context.Context, drifted, normal []*state.StateNode, nodePoolMap map[string]*v1.NodePool) (boundedDrifted, boundedNormal, overflow []*state.StateNode) {
	numNodes, disrupting := r.countNodePoolStats()
	driftBudget := buildBudgetForReason(ctx, nodePoolMap, numNodes, disrupting, r.clock, v1.DisruptionReasonDrifted)
	consolBudget := buildBudgetForReason(ctx, nodePoolMap, numNodes, disrupting, r.clock, v1.DisruptionReasonUnderutilized)

	driftUsed := map[string]int{}
	consolUsed := map[string]int{}
	for _, node := range drifted {
		poolName := node.Labels()[v1.NodePoolLabelKey]
		if driftUsed[poolName] < driftBudget[poolName] {
			boundedDrifted = append(boundedDrifted, node)
			driftUsed[poolName]++
		} else {
			overflow = append(overflow, node)
		}
	}
	for _, node := range normal {
		poolName := node.Labels()[v1.NodePoolLabelKey]
		if consolUsed[poolName] < consolBudget[poolName] {
			boundedNormal = append(boundedNormal, node)
			consolUsed[poolName]++
		} else {
			overflow = append(overflow, node)
		}
	}
	if len(overflow) > 0 {
		log.FromContext(ctx).V(1).WithValues("overflowNodes", len(overflow)).Info("moved nodes exceeding disruption budget to Group D")
	}
	return boundedDrifted, boundedNormal, overflow
}

// countNodePoolStats counts initialized, managed nodes per NodePool and how
// many of those are currently disrupting (NotReady or marked for deletion).
// Mirrors the filtering rules in BuildDisruptionBudgetMapping.
func (r *RankingEngine) countNodePoolStats() (numNodes, disrupting map[string]int) {
	numNodes = map[string]int{}
	disrupting = map[string]int{}
	for _, node := range r.cluster.DeepCopyNodes() {
		if !node.Managed() || !node.Initialized() {
			continue
		}
		if node.NodeClaim != nil && node.NodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue() {
			continue
		}
		poolName := node.Labels()[v1.NodePoolLabelKey]
		numNodes[poolName]++
		if node.Node != nil {
			if cond := nodeutils.GetCondition(node.Node, corev1.NodeReady); cond.Status != corev1.ConditionTrue || node.MarkedForDeletion() {
				disrupting[poolName]++
			}
		}
	}
	return numNodes, disrupting
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

// hasPDBBlockedPods reports whether any of the node's pods is blocked by a
// PDB with DisruptionsAllowed=0. Operates on pre-parsed selectors so the
// per-pod inner loop is allocation-free.
func hasPDBBlockedPods(pods []*corev1.Pod, pdbs []parsedPDB) bool {
	if len(pods) == 0 || len(pdbs) == 0 {
		return false
	}
	for _, pod := range pods {
		podLabels := labels.Set(pod.Labels)
		for i := range pdbs {
			p := &pdbs[i]
			if p.pdb.Namespace != pod.Namespace {
				continue
			}
			if p.pdb.Status.DisruptionsAllowed != 0 {
				continue
			}
			if p.selector.Matches(podLabels) {
				return true
			}
		}
	}
	return false
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

// sortByPodCount sorts nodes by pod count ascending with a deterministic name
// tie-break. Nodes whose pod-list lookup is missing from the map sort to the
// end so a transient lookup failure doesn't skew the top of the ranking.
func (r *RankingEngine) sortByPodCount(nodes []*state.StateNode, nodePods map[string][]*corev1.Pod) {
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
