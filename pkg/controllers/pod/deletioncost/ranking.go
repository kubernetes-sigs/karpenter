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
	"sort"

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

// RankingEngine implements PodCount-based node ranking with three-tier drift partitioning
type RankingEngine struct{}

// NewRankingEngine creates a new RankingEngine
func NewRankingEngine() *RankingEngine {
	return &RankingEngine{}
}

// RankNodes ranks nodes using PodCount strategy with three-tier partitioning:
// Tier 1 (lowest cost): Drifted nodes — sorted by pod count ascending
// Tier 2 (middle):      Normal nodes — sorted by pod count ascending
// Tier 3 (highest):     Do-not-disrupt nodes — sorted by pod count ascending
func (r *RankingEngine) RankNodes(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) ([]NodeRank, error) {
	if len(nodes) == 0 {
		return []NodeRank{}, nil
	}

	defer metrics.Measure(RankingDurationSeconds, map[string]string{})()

	// Partition into three tiers
	drifted, normal, doNotDisrupt, err := partitionNodes(ctx, kubeClient, nodes)
	if err != nil {
		return nil, err
	}

	// Sort each tier by pod count ascending, tiebreak by node name
	sortByPodCount(ctx, kubeClient, drifted)
	sortByPodCount(ctx, kubeClient, normal)
	sortByPodCount(ctx, kubeClient, doNotDisrupt)

	// BaseRank = -n where n is total managed nodes
	baseRank := -len(nodes)
	currentRank := baseRank
	result := make([]NodeRank, 0, len(nodes))

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
		"driftedNodes", len(drifted),
		"normalNodes", len(normal),
		"doNotDisruptNodes", len(doNotDisrupt),
	).Info("completed node ranking")

	return result, nil
}

// partitionNodes splits nodes into three tiers:
// 1. Drifted — NodeClaim has ConditionTypeDrifted=True
// 2. Normal — not drifted, no do-not-disrupt pods
// 3. DoNotDisrupt — has do-not-disrupt pods (regardless of drift)
func partitionNodes(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) (drifted, normal, doNotDisrupt []*state.StateNode, err error) {
	for _, node := range nodes {
		hasDND, err := hasDoNotDisruptPods(ctx, kubeClient, node)
		if err != nil {
			return nil, nil, nil, err
		}
		if hasDND {
			doNotDisrupt = append(doNotDisrupt, node)
		} else if isDrifted(node) {
			drifted = append(drifted, node)
		} else {
			normal = append(normal, node)
		}
	}
	return
}

// isDrifted returns true if the node's NodeClaim has ConditionTypeDrifted=True
func isDrifted(node *state.StateNode) bool {
	if node.NodeClaim == nil {
		return false
	}
	return node.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()
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
