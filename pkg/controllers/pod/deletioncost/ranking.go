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
	"crypto/rand"
	"math/big"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
)

// RankingStrategy defines the algorithm used to rank nodes for pod deletion cost assignment
type RankingStrategy string

const (
	// RankingStrategyRandom assigns node ranks in random order
	RankingStrategyRandom RankingStrategy = "Random"
	// RankingStrategyLargestToSmallest assigns lower ranks to nodes with greater total capacity
	RankingStrategyLargestToSmallest RankingStrategy = "LargestToSmallest"
	// RankingStrategySmallestToLargest assigns lower ranks to nodes with lesser total capacity
	RankingStrategySmallestToLargest RankingStrategy = "SmallestToLargest"
	// RankingStrategyUnallocatedVCPUPerPodCost assigns lower ranks to nodes with higher unallocated vCPU per pod ratios
	RankingStrategyUnallocatedVCPUPerPodCost RankingStrategy = "UnallocatedVCPUPerPodCost"

	// BaseRank is the starting rank for nodes without do-not-disrupt pods
	BaseRank = -1000
)

// NodeRank represents a node with its assigned rank for pod deletion cost
type NodeRank struct {
	Node            *state.StateNode
	Rank            int
	HasDoNotDisrupt bool
}

// RankingEngine implements node ranking strategies
type RankingEngine struct {
	strategy RankingStrategy
}

// NewRankingEngine creates a new RankingEngine with the specified strategy
func NewRankingEngine(strategy RankingStrategy) *RankingEngine {
	return &RankingEngine{
		strategy: strategy,
	}
}

// RankNodes ranks the provided nodes according to the configured strategy
// Returns a slice of NodeRank with assigned ranks
func (r *RankingEngine) RankNodes(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) ([]NodeRank, error) {
	if len(nodes) == 0 {
		return []NodeRank{}, nil
	}

	// Measure ranking duration
	defer metrics.Measure(RankingDurationSeconds, map[string]string{
		strategyLabel: string(r.strategy),
	})()

	// Step 1: Partition nodes by do-not-disrupt status
	groupA, groupB, err := partitionNodesByDoNotDisrupt(ctx, kubeClient, nodes)
	if err != nil {
		return nil, err
	}

	// Step 2: Apply ranking strategy to each group
	var rankedGroupA, rankedGroupB []*state.StateNode

	switch r.strategy {
	case RankingStrategyRandom:
		rankedGroupA = rankNodesRandom(groupA)
		rankedGroupB = rankNodesRandom(groupB)
	case RankingStrategyLargestToSmallest:
		rankedGroupA = rankNodesBySize(groupA, true)
		rankedGroupB = rankNodesBySize(groupB, true)
	case RankingStrategySmallestToLargest:
		rankedGroupA = rankNodesBySize(groupA, false)
		rankedGroupB = rankNodesBySize(groupB, false)
	case RankingStrategyUnallocatedVCPUPerPodCost:
		rankedGroupA, err = rankNodesByUnallocatedVCPUPerPod(ctx, kubeClient, groupA)
		if err != nil {
			return nil, err
		}
		rankedGroupB, err = rankNodesByUnallocatedVCPUPerPod(ctx, kubeClient, groupB)
		if err != nil {
			return nil, err
		}
	default:
		// Default to random if strategy is unknown
		rankedGroupA = rankNodesRandom(groupA)
		rankedGroupB = rankNodesRandom(groupB)
	}

	// Step 3: Assign ranks
	result := make([]NodeRank, 0, len(nodes))
	currentRank := BaseRank

	// Assign ranks to Group A (nodes without do-not-disrupt pods)
	for _, node := range rankedGroupA {
		result = append(result, NodeRank{
			Node:            node,
			Rank:            currentRank,
			HasDoNotDisrupt: false,
		})
		currentRank++
	}

	// Assign ranks to Group B (nodes with do-not-disrupt pods)
	// Start from the next rank after Group A
	for _, node := range rankedGroupB {
		result = append(result, NodeRank{
			Node:            node,
			Rank:            currentRank,
			HasDoNotDisrupt: true,
		})
		currentRank++
	}

	// Record metrics
	NodesRankedTotal.Add(float64(len(result)), map[string]string{
		strategyLabel: string(r.strategy),
	})

	// Log ranking results at debug level
	log.FromContext(ctx).V(1).WithValues(
		"strategy", r.strategy,
		"totalNodes", len(result),
		"nodesWithoutDoNotDisrupt", len(rankedGroupA),
		"nodesWithDoNotDisrupt", len(rankedGroupB),
	).Info("completed node ranking")

	return result, nil
}

// hasDoNotDisruptPods checks if a node has at least one pod with the do-not-disrupt annotation
func hasDoNotDisruptPods(ctx context.Context, kubeClient client.Client, node *state.StateNode) (bool, error) {
	pods, err := node.Pods(ctx, kubeClient)
	if err != nil {
		return false, err
	}

	for _, pod := range pods {
		if podutils.HasDoNotDisrupt(pod) {
			return true, nil
		}
	}

	return false, nil
}

// partitionNodesByDoNotDisrupt partitions nodes into two groups:
// - Group A: nodes without do-not-disrupt pods
// - Group B: nodes with do-not-disrupt pods
func partitionNodesByDoNotDisrupt(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) (groupA, groupB []*state.StateNode, err error) {
	groupA = make([]*state.StateNode, 0)
	groupB = make([]*state.StateNode, 0)

	for _, node := range nodes {
		hasDoNotDisrupt, err := hasDoNotDisruptPods(ctx, kubeClient, node)
		if err != nil {
			return nil, nil, err
		}

		if hasDoNotDisrupt {
			groupB = append(groupB, node)
		} else {
			groupA = append(groupA, node)
		}
	}

	return groupA, groupB, nil
}

// rankNodesRandom shuffles nodes randomly and assigns sequential ranks
func rankNodesRandom(nodes []*state.StateNode) []*state.StateNode {
	if len(nodes) == 0 {
		return nodes
	}

	// Create a copy to avoid modifying the original slice
	shuffled := make([]*state.StateNode, len(nodes))
	copy(shuffled, nodes)

	// Fisher-Yates shuffle using crypto/rand for determinism
	for i := len(shuffled) - 1; i > 0; i-- {
		// Generate random index from 0 to i (inclusive)
		maxBig := big.NewInt(int64(i + 1))
		jBig, err := rand.Int(rand.Reader, maxBig)
		if err != nil {
			// If crypto/rand fails, fall back to no shuffle (deterministic by node order)
			return nodes
		}
		j := int(jBig.Int64())

		// Swap elements
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}

	return shuffled
}

// calculateNormalizedCapacity calculates a normalized capacity value for a node
// by combining CPU and memory resources into a single comparable value
func calculateNormalizedCapacity(allocatable corev1.ResourceList) float64 {
	// Get CPU in millicores
	cpu := allocatable[corev1.ResourceCPU]
	cpuMillis := float64(cpu.MilliValue())

	// Get memory in bytes
	memory := allocatable[corev1.ResourceMemory]
	memoryBytes := float64(memory.Value())

	// Normalize: 1 CPU core = 1GB memory for comparison purposes
	// This gives us a single comparable value
	cpuNormalized := cpuMillis / 1000.0                          // Convert millicores to cores
	memoryNormalized := memoryBytes / (1024.0 * 1024.0 * 1024.0) // Convert bytes to GB

	return cpuNormalized + memoryNormalized
}

// rankNodesBySize sorts nodes by their total allocatable capacity
// If largest is true, sorts largest to smallest (descending)
// If largest is false, sorts smallest to largest (ascending)
func rankNodesBySize(nodes []*state.StateNode, largest bool) []*state.StateNode {
	if len(nodes) == 0 {
		return nodes
	}

	// Create a copy to avoid modifying the original slice
	sorted := make([]*state.StateNode, len(nodes))
	copy(sorted, nodes)

	// Sort by normalized capacity
	sort.SliceStable(sorted, func(i, j int) bool {
		capacityI := calculateNormalizedCapacity(sorted[i].Allocatable())
		capacityJ := calculateNormalizedCapacity(sorted[j].Allocatable())

		if capacityI == capacityJ {
			// Deterministic ordering by node name for ties
			return sorted[i].Name() < sorted[j].Name()
		}

		if largest {
			// Largest to smallest (descending)
			return capacityI > capacityJ
		}
		// Smallest to largest (ascending)
		return capacityI < capacityJ
	})

	return sorted
}

// calculateUnallocatedVCPUPerPod calculates the ratio of unallocated vCPU to pod count
func calculateUnallocatedVCPUPerPod(ctx context.Context, kubeClient client.Client, node *state.StateNode) (float64, error) {
	// Get unallocated CPU (available CPU)
	available := node.Available()
	unallocatedCPU := available[corev1.ResourceCPU]
	unallocatedMillis := float64(unallocatedCPU.MilliValue())

	// Get pod count on the node
	pods, err := node.Pods(ctx, kubeClient)
	if err != nil {
		return 0, err
	}
	podCount := len(pods)

	// Handle edge case: no pods on node
	if podCount == 0 {
		// Return a very high value to indicate this node should be ranked lower
		// (higher deletion priority) since it has no pods
		return 1e9, nil
	}

	// Calculate ratio: unallocated vCPU / pod count
	// Convert millicores to cores for the ratio
	unallocatedCores := unallocatedMillis / 1000.0
	ratio := unallocatedCores / float64(podCount)

	return ratio, nil
}

// rankNodesByUnallocatedVCPUPerPod sorts nodes by their unallocated vCPU per pod ratio
// Higher ratios get lower ranks (higher deletion priority)
func rankNodesByUnallocatedVCPUPerPod(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) ([]*state.StateNode, error) {
	if len(nodes) == 0 {
		return nodes, nil
	}

	// Create a copy to avoid modifying the original slice
	sorted := make([]*state.StateNode, len(nodes))
	copy(sorted, nodes)

	// Calculate ratios for all nodes
	ratios := make(map[string]float64)
	for _, node := range sorted {
		ratio, err := calculateUnallocatedVCPUPerPod(ctx, kubeClient, node)
		if err != nil {
			return nil, err
		}
		ratios[node.Name()] = ratio
	}

	// Sort by ratio descending (higher ratio = lower rank = higher deletion priority)
	sort.SliceStable(sorted, func(i, j int) bool {
		ratioI := ratios[sorted[i].Name()]
		ratioJ := ratios[sorted[j].Name()]

		if ratioI == ratioJ {
			// Deterministic ordering by node name for ties
			return sorted[i].Name() < sorted[j].Name()
		}

		// Higher ratio first (descending)
		return ratioI > ratioJ
	})

	return sorted, nil
}
