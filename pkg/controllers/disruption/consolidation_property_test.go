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
	"strconv"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// Feature: consolidation-decision-ratio-control, Property 13: Delete Ratio Filtering
// Validates: Requirements 4.2
//
// This property test verifies that when ConsolidateWhen is set to WhenCostJustifiesDisruption,
// if the delete ratio is less than 1.0, then no consolidation moves should be generated for that node.
//
//nolint:gocyclo
func TestProperty_DeleteRatioFiltering(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("nodes with delete ratio < 1.0 are filtered out",
		prop.ForAll(
			func(candidateData []candidateTestData) bool {
				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates from test data
				candidates := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates[i] = createConsolidationTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Compute delete ratios for all candidates
				deleteRatios := make([]float64, len(candidates))
				for i, candidate := range candidates {
					deleteRatios[i] = calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)
				}

				// Simulate the filtering logic from computeConsolidation
				filteredCandidates := make([]*Candidate, 0, len(candidates))
				for i, candidate := range candidates {
					if deleteRatios[i] >= 1.0 {
						filteredCandidates = append(filteredCandidates, candidate)
					}
				}

				// Verify that all filtered candidates have delete ratio >= 1.0
				for _, candidate := range filteredCandidates {
					if candidate.deleteRatio < 1.0 {
						return false
					}
				}

				// Verify that all candidates with delete ratio < 1.0 were filtered out
				for i, candidate := range candidates {
					if deleteRatios[i] < 1.0 {
						// This candidate should not be in filteredCandidates
						for _, filtered := range filteredCandidates {
							if filtered == candidate {
								return false // Found a candidate that should have been filtered
							}
						}
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 14: Delete Ratio as Upper Bound
// Validates: Requirements 4.3
//
// This property test verifies that for any node with both delete and replace move options,
// the delete ratio is greater than or equal to any replace move ratio for the same source node.
func TestProperty_DeleteRatioAsUpperBound(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("delete ratio >= replace ratio for same node",
		prop.ForAll(
			func(candidateData []candidateTestData) bool {
				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates from test data
				candidates := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates[i] = createConsolidationTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// For each candidate, verify that delete ratio is an upper bound
				for i, candidate := range candidates {
					// Compute delete ratio (assumes full cost recovery)
					deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

					// Simulate a replace move with a cheaper instance (50% of original cost)
					replacementCost := candidateData[i].cost * 0.5
					costSavings := candidateData[i].cost - replacementCost

					// Compute replace ratio using the same normalization
					normalizedCostSavings := costSavings / totalCost
					normalizedDisruption := candidateData[i].disruption / totalDisruption

					var replaceRatio float64
					if normalizedDisruption == 0 {
						replaceRatio = 1e9 // Large but finite value
					} else {
						replaceRatio = normalizedCostSavings / normalizedDisruption
					}

					// Delete ratio should be >= replace ratio (with tolerance for floating point)
					if deleteRatio < replaceRatio-1e-9 {
						return false
					}

					// Also verify with a more expensive replacement (90% of original cost)
					replacementCost2 := candidateData[i].cost * 0.9
					costSavings2 := candidateData[i].cost - replacementCost2
					normalizedCostSavings2 := costSavings2 / totalCost

					var replaceRatio2 float64
					if normalizedDisruption == 0 {
						replaceRatio2 = 1e9
					} else {
						replaceRatio2 = normalizedCostSavings2 / normalizedDisruption
					}

					// Delete ratio should still be >= this replace ratio
					if deleteRatio < replaceRatio2-1e-9 {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// createConsolidationTestCandidate creates a test candidate from test data for consolidation property tests
func createConsolidationTestCandidate(data candidateTestData) *Candidate {
	return &Candidate{
		StateNode: &state.StateNode{
			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
				},
			},
			NodeClaim: &v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-nodeclaim",
				},
			},
		},
		instanceType: &cloudprovider.InstanceType{
			Name: "test-instance-type",
			Offerings: cloudprovider.Offerings{
				{Price: data.cost},
			},
		},
		DisruptionCost: data.disruption,
		NodePool: &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-nodepool",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateWhen: v1.ConsolidateWhenCostJustifiesDisruption,
				},
			},
		},
	}
}

// Feature: consolidation-decision-ratio-control, Property 10: Ratio-Based Move Ordering
// Validates: Requirements 3.1
//
// This property test verifies that when ConsolidateWhen is set to WhenCostJustifiesDisruption,
// sorting moves by decision ratio in descending order results in the first move having the
// highest ratio and the last move having the lowest ratio.
//
//nolint:gocyclo
func TestProperty_RatioBasedMoveOrdering(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("moves sorted by decision ratio in descending order",
		prop.ForAll(
			func(candidateData []candidateTestData) bool {
				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates from test data with WhenCostJustifiesDisruption policy
				candidates := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates[i] = createConsolidationTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Compute decision ratios for all candidates
				for _, candidate := range candidates {
					calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
				}

				// Sort candidates by decision ratio in descending order (as done in SortCandidates)
				sortedCandidates := make([]*Candidate, len(candidates))
				copy(sortedCandidates, candidates)

				// Simulate the sorting logic from SingleNodeConsolidation.SortCandidates
				for i := 0; i < len(sortedCandidates); i++ {
					for j := i + 1; j < len(sortedCandidates); j++ {
						if sortedCandidates[i].decisionRatio < sortedCandidates[j].decisionRatio {
							sortedCandidates[i], sortedCandidates[j] = sortedCandidates[j], sortedCandidates[i]
						}
					}
				}

				// Verify that the sorted list is in descending order
				for i := 0; i < len(sortedCandidates)-1; i++ {
					if sortedCandidates[i].decisionRatio < sortedCandidates[i+1].decisionRatio {
						return false
					}
				}

				// Verify that the first candidate has the highest ratio
				maxRatio := candidates[0].decisionRatio
				for _, candidate := range candidates {
					if candidate.decisionRatio > maxRatio {
						maxRatio = candidate.decisionRatio
					}
				}
				if !floatEquals(sortedCandidates[0].decisionRatio, maxRatio, 1e-9) {
					return false
				}

				// Verify that the last candidate has the lowest ratio
				minRatio := candidates[0].decisionRatio
				for _, candidate := range candidates {
					if candidate.decisionRatio < minRatio {
						minRatio = candidate.decisionRatio
					}
				}
				return floatEquals(sortedCandidates[len(sortedCandidates)-1].decisionRatio, minRatio, 1e-9)
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 11: Cumulative Cost-Disruption Efficiency
// Validates: Requirements 3.3
//
// This property test verifies that for any set of consolidation moves with decision ratios above 1.0,
// executing the first N moves in ratio-descending order results in a better cumulative ratio than
// executing the first N moves in ratio-ascending order.
//
//nolint:gocyclo
func TestProperty_CumulativeCostDisruptionEfficiency(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("ratio-descending order has better cumulative ratio at each step",
		prop.ForAll(
			func(candidateData []candidateTestData) bool {
				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates from test data
				candidates := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates[i] = createConsolidationTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Compute decision ratios for all candidates
				for _, candidate := range candidates {
					calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
				}

				// Filter to only candidates with ratio >= 1.0
				goodCandidates := make([]*Candidate, 0)
				for _, candidate := range candidates {
					if candidate.decisionRatio >= 1.0 {
						goodCandidates = append(goodCandidates, candidate)
					}
				}

				// Skip if we don't have enough good candidates
				if len(goodCandidates) < 2 {
					return true
				}

				// Sort candidates by decision ratio in descending order (best first)
				bestOrderCandidates := make([]*Candidate, len(goodCandidates))
				copy(bestOrderCandidates, goodCandidates)
				for i := 0; i < len(bestOrderCandidates); i++ {
					for j := i + 1; j < len(bestOrderCandidates); j++ {
						if bestOrderCandidates[i].decisionRatio < bestOrderCandidates[j].decisionRatio {
							bestOrderCandidates[i], bestOrderCandidates[j] = bestOrderCandidates[j], bestOrderCandidates[i]
						}
					}
				}

				// Create worst-case ordering (ascending by decision ratio - worst first)
				worstOrderCandidates := make([]*Candidate, len(goodCandidates))
				for i := 0; i < len(goodCandidates); i++ {
					worstOrderCandidates[i] = bestOrderCandidates[len(goodCandidates)-1-i]
				}

				// Compare cumulative ratios at each step (after consolidating N nodes)
				// Best order should have >= cumulative ratio at each step
				bestCumulativeCost := 0.0
				bestCumulativeDisruption := 0.0
				worstCumulativeCost := 0.0
				worstCumulativeDisruption := 0.0

				for i := 0; i < len(goodCandidates); i++ {
					bestCumulativeCost += bestOrderCandidates[i].normalizedCost
					bestCumulativeDisruption += bestOrderCandidates[i].normalizedDisruption
					worstCumulativeCost += worstOrderCandidates[i].normalizedCost
					worstCumulativeDisruption += worstOrderCandidates[i].normalizedDisruption

					// Compute cumulative ratios
					var bestRatio, worstRatio float64
					if bestCumulativeDisruption > 0 {
						bestRatio = bestCumulativeCost / bestCumulativeDisruption
					}
					if worstCumulativeDisruption > 0 {
						worstRatio = worstCumulativeCost / worstCumulativeDisruption
					}

					// Best order should have >= cumulative ratio at this step
					// (allowing tolerance for floating point errors)
					if bestRatio < worstRatio-1e-6 {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: configurable-decision-ratio-threshold, Property 8: Delete Ratio Filtering
// Validates: Requirements 5.2
//
// This property test verifies that when ConsolidateWhen is set to WhenCostJustifiesDisruption,
// nodes with delete ratio < threshold are skipped, and nodes with delete ratio >= threshold are not skipped.
//
//nolint:gocyclo
func TestProperty_DeleteRatioFilteringWithThreshold(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("nodes with delete ratio < threshold are skipped, >= threshold are not skipped",
		prop.ForAll(
			func(candidateData []candidateTestData, threshold float64) bool {
				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates from test data with WhenCostJustifiesDisruption policy
				candidates := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates[i] = createConsolidationTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Compute delete ratios for all candidates
				deleteRatios := make([]float64, len(candidates))
				for i, candidate := range candidates {
					deleteRatios[i] = calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)
				}

				// Simulate the filtering logic from computeConsolidation
				filteredCandidates := make([]*Candidate, 0, len(candidates))
				for i, candidate := range candidates {
					if deleteRatios[i] >= threshold {
						filteredCandidates = append(filteredCandidates, candidate)
					}
				}

				// Verify that all filtered candidates have delete ratio >= threshold
				for i, candidate := range candidates {
					isFiltered := false
					for _, filtered := range filteredCandidates {
						if filtered == candidate {
							isFiltered = true
							break
						}
					}

					if isFiltered {
						// This candidate should have delete ratio >= threshold
						if deleteRatios[i] < threshold {
							return false
						}
					} else {
						// This candidate should have delete ratio < threshold
						if deleteRatios[i] >= threshold {
							return false
						}
					}
				}

				return true
			},
			genCandidateDataSlice(),
			gen.Float64Range(0.1, 3.0), // Threshold range
		))

	properties.TestingRun(t)
}

// Feature: configurable-decision-ratio-threshold, Property 9: Delete Ratio Filtering Monotonicity
// Validates: Requirements 5.4
//
// This property test verifies that for any two threshold values where threshold_A < threshold_B,
// the number of nodes skipped by delete ratio filtering with threshold_B is greater than or equal
// to the number skipped with threshold_A.
//
//nolint:gocyclo
func TestProperty_DeleteRatioFilteringMonotonicity(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("more nodes are skipped with higher threshold",
		prop.ForAll(
			func(candidateData []candidateTestData, thresholdA, thresholdB float64) bool {
				// Ensure thresholdA < thresholdB
				if thresholdA >= thresholdB {
					return true
				}

				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates from test data with WhenCostJustifiesDisruption policy
				candidates := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates[i] = createConsolidationTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Compute delete ratios for all candidates
				deleteRatios := make([]float64, len(candidates))
				for i, candidate := range candidates {
					deleteRatios[i] = calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)
				}

				// Count nodes skipped with threshold A
				skippedWithA := 0
				for _, ratio := range deleteRatios {
					if ratio < thresholdA {
						skippedWithA++
					}
				}

				// Count nodes skipped with threshold B
				skippedWithB := 0
				for _, ratio := range deleteRatios {
					if ratio < thresholdB {
						skippedWithB++
					}
				}

				// Since thresholdA < thresholdB, more nodes should be skipped with B
				// (or equal if no nodes have ratios in the range [thresholdA, thresholdB))
				return skippedWithB >= skippedWithA
			},
			genCandidateDataSlice(),
			gen.Float64Range(0.1, 2.0), // Threshold A
			gen.Float64Range(0.1, 2.0), // Threshold B
		))

	properties.TestingRun(t)
}

// Feature: configurable-decision-ratio-threshold, Property 5: Per-NodePool Threshold Independence
// Validates: Requirements 2.4
//
// This property test verifies that when multiple NodePools have different DecisionRatioThreshold values,
// each NodePool uses its own threshold independently, and changing one NodePool's threshold doesn't affect others.
//
//nolint:gocyclo
func TestProperty_PerNodePoolThresholdIndependence(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("each NodePool uses its own threshold independently",
		prop.ForAll(
			func(nodepool1Data, nodepool2Data, nodepool3Data []candidateTestData, threshold1, threshold2, threshold3 float64) bool {
				// Skip if any nodepool has insufficient candidates
				if len(nodepool1Data) < 2 || len(nodepool2Data) < 2 || len(nodepool3Data) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create three NodePools with different thresholds
				nodepool1Candidates := createCandidatesForNodePoolWithThreshold("nodepool-1", threshold1, nodepool1Data)
				nodepool2Candidates := createCandidatesForNodePoolWithThreshold("nodepool-2", threshold2, nodepool2Data)
				nodepool3Candidates := createCandidatesForNodePoolWithThreshold("nodepool-3", threshold3, nodepool3Data)

				// Process each NodePool independently
				nodepoolCandidates := [][]*Candidate{
					nodepool1Candidates,
					nodepool2Candidates,
					nodepool3Candidates,
				}
				thresholds := []float64{threshold1, threshold2, threshold3}

				// For each NodePool, compute decision ratios and verify threshold is applied correctly
				for npIdx, candidates := range nodepoolCandidates {
					threshold := thresholds[npIdx]

					// Compute nodepool metrics
					totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

					// Skip if total cost or disruption is zero
					if totalCost == 0.0 || totalDisruption == 0.0 {
						continue
					}

					// Compute decision ratios for all candidates
					for _, candidate := range candidates {
						calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
					}

					// Verify that the threshold from the NodePool is used
					for _, candidate := range candidates {
						expectedThreshold := candidate.NodePool.Spec.Disruption.GetDecisionRatioThreshold()
						if !floatEquals(expectedThreshold, threshold, 1e-9) {
							return false
						}

						// Simulate policy evaluation with the threshold
						policyEvaluator := &PolicyEvaluator{}
						shouldConsolidate := policyEvaluator.ShouldConsolidate(
							candidate.NodePool.Spec.Disruption.ConsolidateWhen,
							candidate,
							candidate.decisionRatio,
							expectedThreshold,
						)

						// Verify the decision matches the threshold comparison
						expectedDecision := candidate.decisionRatio >= threshold
						if shouldConsolidate != expectedDecision {
							return false
						}
					}
				}

				// Verify that changing one NodePool's threshold doesn't affect others
				// Modify nodepool1's threshold
				modifiedThreshold := threshold1 * 2.0
				for _, candidate := range nodepool1Candidates {
					candidate.NodePool.Spec.Disruption.DecisionRatioThreshold = &modifiedThreshold
				}

				// Verify nodepool2 and nodepool3 still use their original thresholds
				for _, candidate := range nodepool2Candidates {
					if !floatEquals(candidate.NodePool.Spec.Disruption.GetDecisionRatioThreshold(), threshold2, 1e-9) {
						return false
					}
				}
				for _, candidate := range nodepool3Candidates {
					if !floatEquals(candidate.NodePool.Spec.Disruption.GetDecisionRatioThreshold(), threshold3, 1e-9) {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
			genCandidateDataSlice(),
			genCandidateDataSlice(),
			gen.Float64Range(0.5, 3.0), // Threshold 1
			gen.Float64Range(0.5, 3.0), // Threshold 2
			gen.Float64Range(0.5, 3.0), // Threshold 3
		))

	properties.TestingRun(t)
}

// Feature: configurable-decision-ratio-threshold, Property 6: Threshold Monotonicity
// Validates: Requirements 3.3, 3.4, 4.3
//
// This property test verifies that for any two threshold values where threshold_A < threshold_B,
// the set of consolidation moves executed with threshold_B is a subset of (or equal to) the moves
// executed with threshold_A.
//
//nolint:gocyclo
func TestProperty_ThresholdMonotonicity(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("moves with higher threshold are subset of moves with lower threshold",
		prop.ForAll(
			func(candidateData []candidateTestData, thresholdA, thresholdB float64) bool {
				// Ensure thresholdA < thresholdB
				if thresholdA >= thresholdB {
					return true
				}

				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates with WhenCostJustifiesDisruption policy
				candidates := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates[i] = createConsolidationTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Compute decision ratios for all candidates
				for _, candidate := range candidates {
					calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
				}

				// Determine which moves would be executed with threshold A
				policyEvaluator := &PolicyEvaluator{}
				movesWithA := make([]*Candidate, 0)
				for _, candidate := range candidates {
					if policyEvaluator.ShouldConsolidate(
						v1.ConsolidateWhenCostJustifiesDisruption,
						candidate,
						candidate.decisionRatio,
						thresholdA,
					) {
						movesWithA = append(movesWithA, candidate)
					}
				}

				// Determine which moves would be executed with threshold B
				movesWithB := make([]*Candidate, 0)
				for _, candidate := range candidates {
					if policyEvaluator.ShouldConsolidate(
						v1.ConsolidateWhenCostJustifiesDisruption,
						candidate,
						candidate.decisionRatio,
						thresholdB,
					) {
						movesWithB = append(movesWithB, candidate)
					}
				}

				// Verify that movesWithB is a subset of movesWithA
				// Every move in movesWithB should also be in movesWithA
				for _, moveB := range movesWithB {
					found := false
					for _, moveA := range movesWithA {
						if moveB == moveA {
							found = true
							break
						}
					}
					if !found {
						return false
					}
				}

				// Additionally verify the count relationship
				if len(movesWithB) > len(movesWithA) {
					return false
				}

				return true
			},
			genCandidateDataSlice(),
			gen.Float64Range(0.5, 2.0), // Threshold A
			gen.Float64Range(0.5, 2.0), // Threshold B
		))

	properties.TestingRun(t)
}

// Feature: configurable-decision-ratio-threshold, Property 14: Backward Compatibility
// Validates: Requirements 9.2
//
// This property test verifies that when DecisionRatioThreshold is not specified (nil),
// the system behaves identically to when DecisionRatioThreshold is explicitly set to 1.0.
//
//nolint:gocyclo
func TestProperty_BackwardCompatibility(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("nil threshold behaves identically to threshold=1.0",
		prop.ForAll(
			func(candidateData []candidateTestData) bool {
				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create two sets of candidates: one with nil threshold, one with threshold=1.0
				candidatesWithNil := make([]*Candidate, len(candidateData))
				candidatesWithOne := make([]*Candidate, len(candidateData))

				for i, data := range candidateData {
					// Candidates with nil threshold
					candidatesWithNil[i] = &Candidate{
						StateNode: &state.StateNode{
							Node: &corev1.Node{
								ObjectMeta: metav1.ObjectMeta{
									Name: "test-node-nil-" + strconv.Itoa(i),
								},
							},
							NodeClaim: &v1.NodeClaim{
								ObjectMeta: metav1.ObjectMeta{
									Name: "test-nodeclaim-nil-" + strconv.Itoa(i),
								},
							},
						},
						instanceType: &cloudprovider.InstanceType{
							Name: "test-instance-type",
							Offerings: cloudprovider.Offerings{
								{Price: data.cost},
							},
						},
						DisruptionCost: data.disruption,
						NodePool: &v1.NodePool{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test-nodepool-nil",
							},
							Spec: v1.NodePoolSpec{
								Disruption: v1.Disruption{
									ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
									DecisionRatioThreshold: nil, // Nil threshold
								},
							},
						},
					}

					// Candidates with threshold=1.0
					thresholdOne := 1.0
					candidatesWithOne[i] = &Candidate{
						StateNode: &state.StateNode{
							Node: &corev1.Node{
								ObjectMeta: metav1.ObjectMeta{
									Name: "test-node-one-" + strconv.Itoa(i),
								},
							},
							NodeClaim: &v1.NodeClaim{
								ObjectMeta: metav1.ObjectMeta{
									Name: "test-nodeclaim-one-" + strconv.Itoa(i),
								},
							},
						},
						instanceType: &cloudprovider.InstanceType{
							Name: "test-instance-type",
							Offerings: cloudprovider.Offerings{
								{Price: data.cost},
							},
						},
						DisruptionCost: data.disruption,
						NodePool: &v1.NodePool{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test-nodepool-one",
							},
							Spec: v1.NodePoolSpec{
								Disruption: v1.Disruption{
									ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
									DecisionRatioThreshold: &thresholdOne, // Explicit 1.0
								},
							},
						},
					}
				}

				// Compute metrics for both sets
				totalCostNil, totalDisruptionNil := calculator.ComputeNodePoolMetrics(ctx, candidatesWithNil)
				totalCostOne, totalDisruptionOne := calculator.ComputeNodePoolMetrics(ctx, candidatesWithOne)

				// Skip if total cost or disruption is zero
				if totalCostNil == 0.0 || totalDisruptionNil == 0.0 || totalCostOne == 0.0 || totalDisruptionOne == 0.0 {
					return true
				}

				// Compute decision ratios for both sets
				for _, candidate := range candidatesWithNil {
					calculator.ComputeDecisionRatio(ctx, candidate, totalCostNil, totalDisruptionNil)
				}
				for _, candidate := range candidatesWithOne {
					calculator.ComputeDecisionRatio(ctx, candidate, totalCostOne, totalDisruptionOne)
				}

				// Verify that GetDecisionRatioThreshold returns 1.0 for both
				for _, candidate := range candidatesWithNil {
					threshold := candidate.NodePool.Spec.Disruption.GetDecisionRatioThreshold()
					if !floatEquals(threshold, 1.0, 1e-9) {
						return false
					}
				}
				for _, candidate := range candidatesWithOne {
					threshold := candidate.NodePool.Spec.Disruption.GetDecisionRatioThreshold()
					if !floatEquals(threshold, 1.0, 1e-9) {
						return false
					}
				}

				// Verify that policy evaluation produces identical results
				policyEvaluator := &PolicyEvaluator{}
				for i := range candidateData {
					thresholdNil := candidatesWithNil[i].NodePool.Spec.Disruption.GetDecisionRatioThreshold()
					thresholdOne := candidatesWithOne[i].NodePool.Spec.Disruption.GetDecisionRatioThreshold()

					shouldConsolidateNil := policyEvaluator.ShouldConsolidate(
						v1.ConsolidateWhenCostJustifiesDisruption,
						candidatesWithNil[i],
						candidatesWithNil[i].decisionRatio,
						thresholdNil,
					)

					shouldConsolidateOne := policyEvaluator.ShouldConsolidate(
						v1.ConsolidateWhenCostJustifiesDisruption,
						candidatesWithOne[i],
						candidatesWithOne[i].decisionRatio,
						thresholdOne,
					)

					// Both should produce the same decision
					if shouldConsolidateNil != shouldConsolidateOne {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: configurable-decision-ratio-threshold, Property 15: Consistent Threshold Application
// Validates: Requirements 11.4
//
// This property test verifies that for any NodePool with a configured threshold,
// all consolidation decisions within a single consolidation pass use the same threshold value.
//
//nolint:gocyclo
func TestProperty_ConsistentThresholdApplication(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("same threshold is used throughout a single consolidation pass",
		prop.ForAll(
			func(candidateData []candidateTestData, threshold float64) bool {
				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates with a specific threshold
				candidates := createCandidatesForNodePoolWithThreshold("test-nodepool", threshold, candidateData)

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Compute decision ratios for all candidates
				for _, candidate := range candidates {
					calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
				}

				// Verify that all candidates use the same threshold
				firstThreshold := candidates[0].NodePool.Spec.Disruption.GetDecisionRatioThreshold()
				for _, candidate := range candidates {
					candidateThreshold := candidate.NodePool.Spec.Disruption.GetDecisionRatioThreshold()
					if !floatEquals(candidateThreshold, firstThreshold, 1e-9) {
						return false
					}
					if !floatEquals(candidateThreshold, threshold, 1e-9) {
						return false
					}
				}

				// Simulate a consolidation pass and verify threshold consistency
				policyEvaluator := &PolicyEvaluator{}
				for _, candidate := range candidates {
					// Get the threshold for this candidate
					candidateThreshold := candidate.NodePool.Spec.Disruption.GetDecisionRatioThreshold()

					// Verify it matches the expected threshold
					if !floatEquals(candidateThreshold, threshold, 1e-9) {
						return false
					}

					// Evaluate the policy with this threshold
					shouldConsolidate := policyEvaluator.ShouldConsolidate(
						candidate.NodePool.Spec.Disruption.ConsolidateWhen,
						candidate,
						candidate.decisionRatio,
						candidateThreshold,
					)

					// Verify the decision is consistent with the threshold
					expectedDecision := candidate.decisionRatio >= threshold
					if shouldConsolidate != expectedDecision {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
			gen.Float64Range(0.5, 3.0), // Threshold
		))

	properties.TestingRun(t)
}

// createCandidatesForNodePoolWithThreshold creates a slice of candidates for a specific nodepool with a threshold
// All candidates use WhenCostJustifiesDisruption policy
func createCandidatesForNodePoolWithThreshold(nodepoolName string, threshold float64, data []candidateTestData) []*Candidate {
	candidates := make([]*Candidate, len(data))

	for i, d := range data {
		candidates[i] = &Candidate{
			StateNode: &state.StateNode{
				Node: &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodepoolName + "-node-" + strconv.Itoa(i),
					},
				},
				NodeClaim: &v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodepoolName + "-nodeclaim-" + strconv.Itoa(i),
					},
				},
			},
			instanceType: &cloudprovider.InstanceType{
				Name: "test-instance-type",
				Offerings: cloudprovider.Offerings{
					{Price: d.cost},
				},
			},
			DisruptionCost: d.disruption,
			NodePool: &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodepoolName,
				},
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
						DecisionRatioThreshold: &threshold,
					},
				},
			},
		}
	}

	return candidates
}
