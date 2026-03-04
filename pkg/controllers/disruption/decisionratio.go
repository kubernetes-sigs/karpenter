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
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"

	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
)

// DecisionRatioCalculator computes decision ratios for consolidation candidates.
// The decision ratio quantifies the cost-benefit tradeoff of consolidation moves by
// comparing normalized cost savings to normalized disruption cost.
type DecisionRatioCalculator struct {
	clock clock.Clock
}

// NewDecisionRatioCalculator creates a new DecisionRatioCalculator
func NewDecisionRatioCalculator(clk clock.Clock) *DecisionRatioCalculator {
	return &DecisionRatioCalculator{
		clock: clk,
	}
}

// ComputeNodePoolMetrics computes aggregate metrics for a nodepool by summing
// the total cost and total disruption across all candidates.
//
// Returns:
//   - totalCost: Sum of hourly costs for all candidates
//   - totalDisruption: Sum of disruption costs for all candidates
//
// Edge cases:
//   - If candidates is empty, returns (0.0, 0.0)
//   - If any candidate has nil instanceType or no offerings, that candidate's cost is treated as 0
//   - Handles the case where totalCost is zero by returning zeros (caller should skip consolidation)
func (c *DecisionRatioCalculator) ComputeNodePoolMetrics(
	ctx context.Context,
	candidates []*Candidate,
) (totalCost, totalDisruption float64) {
	if len(candidates) == 0 {
		return 0.0, 0.0
	}

	for _, candidate := range candidates {
		// Sum the hourly cost from the instance type's first offering
		if candidate.instanceType != nil && len(candidate.instanceType.Offerings) > 0 {
			totalCost += candidate.instanceType.Offerings[0].Price
		}
		// Sum the disruption cost
		totalDisruption += candidate.DisruptionCost
	}

	return totalCost, totalDisruption
}

// ComputeDecisionRatio computes the decision ratio for a single candidate.
// The decision ratio quantifies the cost-benefit tradeoff by comparing normalized
// cost savings to normalized disruption cost.
//
// Parameters:
//   - candidate: The consolidation candidate to evaluate
//   - totalCost: Sum of hourly costs for all candidates in the nodepool
//   - totalDisruption: Sum of disruption costs for all candidates in the nodepool
//
// Returns:
//   - decisionRatio: The ratio of normalized cost to normalized disruption
//
// Edge cases:
//   - If totalDisruption is zero, returns positive infinity (consolidation is "free")
//   - If normalizedDisruption is zero, returns positive infinity
//   - If totalCost is zero, the caller should have already skipped consolidation
//
// The decision ratio is computed as:
//
//	normalizedCost = nodeCost / totalCost
//	normalizedDisruption = nodeDisruption / totalDisruption
//	decisionRatio = normalizedCost / normalizedDisruption
//
// A ratio >= 1.0 indicates that cost savings justify the disruption cost.
func (c *DecisionRatioCalculator) ComputeDecisionRatio(
	ctx context.Context,
	candidate *Candidate,
	totalCost, totalDisruption float64,
) float64 {
	// Handle edge case: zero total disruption means consolidation is "free"
	if totalDisruption == 0 {
		return math.Inf(1) // Positive infinity
	}

	// Get the node's hourly cost from the instance type's first offering
	nodeCost := 0.0
	if candidate.instanceType != nil && len(candidate.instanceType.Offerings) > 0 {
		nodeCost = candidate.instanceType.Offerings[0].Price
	}

	// Compute normalized cost
	normalizedCost := nodeCost / totalCost

	// Compute normalized disruption
	normalizedDisruption := candidate.DisruptionCost / totalDisruption

	// Handle edge case: zero normalized disruption
	if normalizedDisruption == 0 {
		return math.Inf(1) // Positive infinity
	}

	// Compute decision ratio
	decisionRatio := normalizedCost / normalizedDisruption

	// Store computed values in the candidate for later use
	candidate.normalizedCost = normalizedCost
	candidate.normalizedDisruption = normalizedDisruption
	candidate.decisionRatio = decisionRatio

	return decisionRatio
}

// ComputeDeleteRatio computes the upper bound decision ratio for a candidate
// assuming full node cost recovery (i.e., a delete move where the entire node
// cost is saved).
//
// This is used as an optimization to skip expensive move generation for nodes
// that cannot produce worthwhile consolidation moves. Since a delete move
// recovers the full node cost, it represents the best-case scenario. If even
// the delete ratio doesn't meet the policy threshold, then no replace move
// (which recovers less cost) will meet the threshold either.
//
// Parameters:
//   - candidate: The consolidation candidate to evaluate
//   - totalCost: Sum of hourly costs for all candidates in the nodepool
//   - totalDisruption: Sum of disruption costs for all candidates in the nodepool
//
// Returns:
//   - deleteRatio: The upper bound decision ratio for this candidate
//
// Edge cases:
//   - If totalDisruption is zero, returns positive infinity (consolidation is "free")
//   - If normalizedDisruption is zero, returns positive infinity
//   - If totalCost is zero, the caller should have already skipped consolidation
//
// The delete ratio uses the same formula as the decision ratio:
//
//	normalizedCost = nodeCost / totalCost
//	normalizedDisruption = nodeDisruption / totalDisruption
//	deleteRatio = normalizedCost / normalizedDisruption
func (c *DecisionRatioCalculator) ComputeDeleteRatio(
	ctx context.Context,
	candidate *Candidate,
	totalCost, totalDisruption float64,
) float64 {
	// Handle edge case: zero total disruption means consolidation is "free"
	if totalDisruption == 0 {
		return math.Inf(1) // Positive infinity
	}

	// Get the node's hourly cost from the instance type's first offering
	nodeCost := 0.0
	if candidate.instanceType != nil && len(candidate.instanceType.Offerings) > 0 {
		nodeCost = candidate.instanceType.Offerings[0].Price
	}

	// Compute normalized cost (same as ComputeDecisionRatio)
	normalizedCost := nodeCost / totalCost

	// Compute normalized disruption (same as ComputeDecisionRatio)
	normalizedDisruption := candidate.DisruptionCost / totalDisruption

	// Handle edge case: zero normalized disruption
	if normalizedDisruption == 0 {
		return math.Inf(1) // Positive infinity
	}

	// Compute delete ratio (same formula as decision ratio)
	deleteRatio := normalizedCost / normalizedDisruption

	// Store computed value in the candidate for later use
	candidate.deleteRatio = deleteRatio

	return deleteRatio
}

// ComputeNodeDisruptionCost computes the total disruption cost for a node by
// summing the eviction costs of all pods on the node and adding a baseline
// node disruption cost.
//
// The disruption cost represents the operational cost of consolidating a node,
// including the cost of evicting its pods and the overhead of the node operation itself.
//
// Parameters:
//   - ctx: Context for logging and cancellation
//   - pods: List of pods on the node
//
// Returns:
//   - disruptionCost: The total disruption cost for the node
//
// The disruption cost is computed as:
//
//	disruptionCost = baselineNodeCost + sum(max(0, EvictionCost(pod)) for pod in pods)
//
// Where:
//   - baselineNodeCost = 1.0 (constant representing the operational cost of consolidating a node)
//   - EvictionCost(pod) is the per-pod eviction cost from pkg/utils/disruption/disruption.go
//   - Negative eviction costs are clamped to zero before summing
//
// Edge cases:
//   - If pods is empty, returns 1.0 (baseline cost only)
//   - Negative eviction costs are clamped to zero to prevent reducing total disruption cost
//
// Validates Requirements: 1.4, 1.5, 1.6, 5.1, 5.2, 5.3
func ComputeNodeDisruptionCost(ctx context.Context, pods []*corev1.Pod) float64 {
	// Start with baseline node disruption cost
	const baselineNodeCost = 1.0
	disruptionCost := baselineNodeCost

	// Sum per-pod eviction costs, clamping negative costs to zero
	for _, pod := range pods {
		evictionCost := disruptionutils.EvictionCost(ctx, pod)
		// Clamp negative costs to zero before adding to total
		if evictionCost > 0 {
			disruptionCost += evictionCost
		}
	}

	return disruptionCost
}
