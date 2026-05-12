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
	"math"
	"sort"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// NodeRank represents a node with its assigned rank for pod deletion cost
type NodeRank struct {
	Node            *state.StateNode
	Rank            int
	HasDoNotDisrupt bool
}

// RankingEngine implements PodCount-based node ranking with four-tier partitioning
type RankingEngine struct{}

// NewRankingEngine creates a new RankingEngine
func NewRankingEngine() *RankingEngine {
	return &RankingEngine{}
}

// RankNodes ranks nodes using PodCount strategy with four-tier partitioning:
// Group A (lowest cost): Disrupted + PDB-blocked nodes, sorted by pod count ascending
// Group B (low cost):    Drifted nodes, sorted by pod count ascending
// Group C (middle):      Normal nodes, sorted by pod count ascending
// Group D (highest):     Do-not-disrupt nodes, sorted by pod count ascending
//
// nodePoolMap provides a mapping from NodePool name to NodePool object for
// budget and consolidation policy checks.
func (r *RankingEngine) RankNodes(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool) ([]NodeRank, error) {
	if len(nodes) == 0 {
		return []NodeRank{}, nil
	}

	defer metrics.Measure(RankingDurationSeconds, map[string]string{})()

	disruptedBlocked, drifted, normal, doNotDisrupt, err := partitionNodes(ctx, kubeClient, nodes, nodePoolMap)
	if err != nil {
		return nil, err
	}

	sortByPodCount(ctx, kubeClient, disruptedBlocked)
	sortByPodCount(ctx, kubeClient, drifted)
	sortByPodCount(ctx, kubeClient, normal)
	sortByPodCount(ctx, kubeClient, doNotDisrupt)

	// Group A nodes (disrupted+PDB-blocked) all get math.MinInt32 so they
	// do not count against the annotation budget.
	remaining := len(drifted) + len(normal) + len(doNotDisrupt)
	baseRank := -remaining
	currentRank := baseRank
	result := make([]NodeRank, 0, len(nodes))

	for _, node := range disruptedBlocked {
		result = append(result, NodeRank{Node: node, Rank: math.MinInt32, HasDoNotDisrupt: false})
	}
	for _, node := range drifted {
		result = append(result, NodeRank{Node: node, Rank: currentRank, HasDoNotDisrupt: false})
		currentRank++
	}
	for _, node := range normal {
		result = append(result, NodeRank{Node: node, Rank: currentRank, HasDoNotDisrupt: false})
		currentRank++
	}
	for _, node := range doNotDisrupt {
		result = append(result, NodeRank{Node: node, Rank: currentRank, HasDoNotDisrupt: true})
		currentRank++
	}

	NodesRankedTotal.Add(float64(len(result)), map[string]string{})

	log.FromContext(ctx).V(1).WithValues(
		"totalNodes", len(result),
		"disruptedBlockedNodes", len(disruptedBlocked),
		"driftedNodes", len(drifted),
		"normalNodes", len(normal),
		"doNotDisruptNodes", len(doNotDisrupt),
	).Info("completed node ranking")

	return result, nil
}

// partitionNodes splits nodes into four tiers:
// A. DisruptedBlocked - has karpenter.sh/disrupted taint AND at least one pod blocked by a PDB
// B. Drifted - NodeClaim has ConditionTypeDrifted=True (but not disrupted+blocked)
// C. Normal - not drifted, no do-not-disrupt pods
// D. DoNotDisrupt - has node-level do-not-disrupt annotation, do-not-disrupt pods,
//
//	or belongs to a NodePool with consolidation disabled
func partitionNodes(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode, nodePoolMap map[string]*v1.NodePool) (disruptedBlocked, drifted, normal, doNotDisrupt []*state.StateNode, err error) {
	for _, node := range nodes {
		// Node-level do-not-disrupt annotation sends to Group D
		if hasNodeDoNotDisrupt(node) {
			doNotDisrupt = append(doNotDisrupt, node)
			continue
		}
		// NodePool with consolidateAfter=Never (nil Duration) means consolidation
		// is disabled, so the node belongs in Group D
		if isConsolidationDisabled(node, nodePoolMap) {
			doNotDisrupt = append(doNotDisrupt, node)
			continue
		}
		hasDND, err := hasDoNotDisruptPods(ctx, kubeClient, node)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if hasDND {
			doNotDisrupt = append(doNotDisrupt, node)
			continue
		}
		if isDisrupted(node) {
			blocked, err := hasPDBBlockedPods(ctx, kubeClient, node)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if blocked {
				disruptedBlocked = append(disruptedBlocked, node)
				continue
			}
		}
		if isDrifted(node) {
			drifted = append(drifted, node)
		} else {
			normal = append(normal, node)
		}
	}
	return
}

// hasNodeDoNotDisrupt returns true if the Node itself has the karpenter.sh/do-not-disrupt annotation.
func hasNodeDoNotDisrupt(node *state.StateNode) bool {
	annotations := node.Annotations()
	if annotations == nil {
		return false
	}
	return annotations[v1.DoNotDisruptAnnotationKey] == "true"
}

// isConsolidationDisabled returns true if the node's NodePool has consolidateAfter
// set to Never (nil Duration), meaning consolidation is disabled for that pool.
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
	_, found := lo.Find(node.Node.Spec.Taints, func(t corev1.Taint) bool {
		return t.MatchTaint(&v1.DisruptedNoScheduleTaint)
	})
	return found
}

// isDrifted returns true if the node's NodeClaim has ConditionTypeDrifted=True
func isDrifted(node *state.StateNode) bool {
	if node.NodeClaim == nil {
		return false
	}
	return node.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()
}

// hasPDBBlockedPods checks if a node has at least one pod whose eviction is blocked by a PDB.
func hasPDBBlockedPods(ctx context.Context, kubeClient client.Client, node *state.StateNode) (bool, error) {
	pods, err := node.Pods(ctx, kubeClient)
	if err != nil {
		return false, err
	}
	if len(pods) == 0 {
		return false, nil
	}

	var pdbList policyv1.PodDisruptionBudgetList
	if err := kubeClient.List(ctx, &pdbList); err != nil {
		return false, err
	}

	for _, pod := range pods {
		for i := range pdbList.Items {
			pdb := &pdbList.Items[i]
			if pdb.Namespace != pod.Namespace {
				continue
			}
			selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
			if err != nil {
				continue
			}
			if selector.Matches(labels.Set(pod.Labels)) && pdb.Status.DisruptionsAllowed == 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// hasDoNotDisruptPods checks if a node has at least one pod with the do-not-disrupt annotation
func hasDoNotDisruptPods(ctx context.Context, kubeClient client.Client, node *state.StateNode) (bool, error) {
	pods, err := node.Pods(ctx, kubeClient)
	if err != nil {
		return false, err
	}
	for _, pod := range pods {
		if pod.Annotations[v1.DoNotDisruptAnnotationKey] == "true" {
			return true, nil
		}
	}
	return false, nil
}

// sortByPodCount sorts nodes by pod count ascending with deterministic tiebreak by node name.
// Pod count errors are treated as 0 (node sorts first).
func sortByPodCount(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) {
	if len(nodes) <= 1 {
		return
	}
	counts := make(map[string]int, len(nodes))
	for _, node := range nodes {
		pods, err := node.Pods(ctx, kubeClient)
		if err != nil {
			counts[node.Name()] = 0
			continue
		}
		counts[node.Name()] = len(pods)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		ci, cj := counts[nodes[i].Name()], counts[nodes[j].Name()]
		if ci != cj {
			return ci < cj
		}
		return nodes[i].Name() < nodes[j].Name()
	})
}
