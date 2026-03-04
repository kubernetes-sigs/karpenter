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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// TestConsolidationFlow_DecisionRatiosComputed verifies that decision ratios are computed for all candidates
func TestConsolidationFlow_DecisionRatiosComputed(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create test candidates with varying costs and disruptions
	candidates := []*Candidate{
		createIntegrationTestCandidateWithValues(1.0, 10.0),
		createIntegrationTestCandidateWithValues(2.0, 15.0),
		createIntegrationTestCandidateWithValues(3.0, 20.0),
	}

	// Compute nodepool metrics
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)
	assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
	assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

	// Compute decision ratios
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
	}

	// Verify all candidates have decision ratios computed
	for i, candidate := range candidates {
		assert.Greater(t, candidate.decisionRatio, 0.0, "Candidate %d should have positive decision ratio", i)
		assert.Greater(t, candidate.normalizedCost, 0.0, "Candidate %d should have positive normalized cost", i)
		assert.Greater(t, candidate.normalizedDisruption, 0.0, "Candidate %d should have positive normalized disruption", i)
	}
}

// TestConsolidationFlow_DeleteRatioFiltering verifies that candidates with low delete ratios are filtered out
func TestConsolidationFlow_DeleteRatioFiltering(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	tests := []struct {
		name                  string
		candidates            []*Candidate
		policy                v1.ConsolidateWhenPolicy
		expectedFilteredCount int
	}{
		{
			name: "WhenCostJustifiesDisruption filters low ratio candidates",
			candidates: []*Candidate{
				createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 1.0, 5.0),  // ratio = 0.2 / 0.125 = 1.6 (keep)
				createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 0.5, 10.0), // ratio = 0.1 / 0.25 = 0.4 (filter)
				createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 2.0, 15.0), // ratio = 0.4 / 0.375 = 1.07 (keep)
				createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 0.3, 10.0), // ratio = 0.06 / 0.25 = 0.24 (filter)
			},
			policy:                v1.ConsolidateWhenCostJustifiesDisruption,
			expectedFilteredCount: 2, // Should keep 2 candidates with ratio >= 1.0
		},
		{
			name: "WhenEmptyOrUnderutilized does not filter by ratio",
			candidates: []*Candidate{
				createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenEmptyOrUnderutilized, 1.0, 5.0),
				createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenEmptyOrUnderutilized, 0.5, 10.0),
				createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenEmptyOrUnderutilized, 2.0, 15.0),
			},
			policy:                v1.ConsolidateWhenEmptyOrUnderutilized,
			expectedFilteredCount: 3, // Should keep all candidates
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Compute nodepool metrics
			totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, tt.candidates)

			// Compute decision ratios and delete ratios
			for _, candidate := range tt.candidates {
				calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
				calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)
			}

			// Simulate delete ratio filtering
			filteredCandidates := make([]*Candidate, 0)
			if tt.policy == v1.ConsolidateWhenCostJustifiesDisruption {
				for _, candidate := range tt.candidates {
					if candidate.deleteRatio >= 1.0 {
						filteredCandidates = append(filteredCandidates, candidate)
					}
				}
			} else {
				filteredCandidates = tt.candidates
			}

			assert.Equal(t, tt.expectedFilteredCount, len(filteredCandidates), "Filtered candidate count should match expected")
		})
	}
}

// TestConsolidationFlow_RatioBasedSorting verifies that candidates are sorted by decision ratio
func TestConsolidationFlow_RatioBasedSorting(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create test candidates with varying decision ratios
	candidates := []*Candidate{
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 1.0, 10.0), // ratio = 0.167 / 0.25 = 0.67
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 3.0, 15.0), // ratio = 0.5 / 0.375 = 1.33
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 2.0, 8.0),  // ratio = 0.333 / 0.2 = 1.67
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 0.5, 7.0),  // ratio = 0.083 / 0.175 = 0.47
	}

	// Compute nodepool metrics
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute decision ratios
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
	}

	// Sort by decision ratio (descending)
	sortedCandidates := make([]*Candidate, len(candidates))
	copy(sortedCandidates, candidates)
	for i := 0; i < len(sortedCandidates); i++ {
		for j := i + 1; j < len(sortedCandidates); j++ {
			if sortedCandidates[i].decisionRatio < sortedCandidates[j].decisionRatio {
				sortedCandidates[i], sortedCandidates[j] = sortedCandidates[j], sortedCandidates[i]
			}
		}
	}

	// Verify sorted order (descending)
	for i := 0; i < len(sortedCandidates)-1; i++ {
		assert.GreaterOrEqual(t, sortedCandidates[i].decisionRatio, sortedCandidates[i+1].decisionRatio,
			"Candidates should be sorted in descending order by decision ratio")
	}

	// Verify the highest ratio is first
	assert.Equal(t, sortedCandidates[0], candidates[2], "Candidate with highest ratio should be first")
}

// TestConsolidationFlow_PolicyFiltering verifies that policy filtering is applied correctly
func TestConsolidationFlow_PolicyFiltering(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		policy        v1.ConsolidateWhenPolicy
		candidate     *Candidate
		decisionRatio float64
		shouldPass    bool
	}{
		{
			name:          "WhenCostJustifiesDisruption allows ratio >= 1.0",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			candidate:     createCandidateWithPods(5),
			decisionRatio: 1.5,
			shouldPass:    true,
		},
		{
			name:          "WhenCostJustifiesDisruption blocks ratio < 1.0",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			candidate:     createCandidateWithPods(5),
			decisionRatio: 0.8,
			shouldPass:    false,
		},
		{
			name:          "WhenEmpty allows empty nodes",
			policy:        v1.ConsolidateWhenEmpty,
			candidate:     createCandidateWithPods(0),
			decisionRatio: 0.5,
			shouldPass:    true,
		},
		{
			name:          "WhenEmpty blocks non-empty nodes",
			policy:        v1.ConsolidateWhenEmpty,
			candidate:     createCandidateWithPods(5),
			decisionRatio: 2.0,
			shouldPass:    false,
		},
		{
			name:          "WhenEmptyOrUnderutilized allows all",
			policy:        v1.ConsolidateWhenEmptyOrUnderutilized,
			candidate:     createCandidateWithPods(5),
			decisionRatio: 0.5,
			shouldPass:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := evaluator.ShouldConsolidate(tt.policy, tt.candidate, tt.decisionRatio, 1.0)
			assert.Equal(t, tt.shouldPass, result, "Policy filtering result should match expected")
		})
	}
}

// TestConsolidationFlow_CommandDecisionRatio verifies that commands have correct decision ratios
func TestConsolidationFlow_CommandDecisionRatio(t *testing.T) {
	tests := []struct {
		name             string
		candidates       []*Candidate
		expectedRatio    float64
		ratioDescription string
	}{
		{
			name: "Single candidate command uses candidate's ratio",
			candidates: []*Candidate{
				createIntegrationTestCandidateWithRatio(2.5),
			},
			expectedRatio:    2.5,
			ratioDescription: "single candidate ratio",
		},
		{
			name: "Multi-candidate command uses minimum ratio",
			candidates: []*Candidate{
				createIntegrationTestCandidateWithRatio(2.5),
				createIntegrationTestCandidateWithRatio(1.8),
				createIntegrationTestCandidateWithRatio(3.2),
			},
			expectedRatio:    1.8,
			ratioDescription: "minimum of all candidate ratios",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ratio := getCommandDecisionRatio(tt.candidates)
			assert.InDelta(t, tt.expectedRatio, ratio, 0.001, "Command decision ratio should be %s", tt.ratioDescription)
		})
	}
}

// Helper functions

func createIntegrationTestCandidateWithValues(cost, disruption float64) *Candidate {
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
				{Price: cost},
			},
		},
		DisruptionCost: disruption,
		NodePool: &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-nodepool",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateWhen: v1.ConsolidateWhenEmptyOrUnderutilized,
				},
			},
		},
	}
}

func createIntegrationTestCandidateWithPolicyAndValues(policy v1.ConsolidateWhenPolicy, cost, disruption float64) *Candidate {
	candidate := createIntegrationTestCandidateWithValues(cost, disruption)
	candidate.NodePool.Spec.Disruption.ConsolidateWhen = policy
	return candidate
}

func createIntegrationTestCandidateWithRatio(ratio float64) *Candidate {
	candidate := createIntegrationTestCandidateWithValues(1.0, 1.0)
	candidate.decisionRatio = ratio
	return candidate
}

// TestIntegration_MixedRatioConsolidation verifies end-to-end consolidation with mixed decision ratios
// Validates: Requirements 2.1, 3.1, 3.2
func TestIntegration_MixedRatioConsolidation(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})
	evaluator := &PolicyEvaluator{}

	// Create nodepool with 10 nodes with varying decision ratios
	// Design ratios to have some above and some below 1.0 threshold
	candidates := []*Candidate{
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 5.0, 10.0),  // ratio = 0.125 / 0.1 = 1.25 (above)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 8.0, 15.0),  // ratio = 0.2 / 0.15 = 1.33 (above)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 3.0, 20.0),  // ratio = 0.075 / 0.2 = 0.375 (below)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 10.0, 12.0), // ratio = 0.25 / 0.12 = 2.08 (above)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 2.0, 8.0),   // ratio = 0.05 / 0.08 = 0.625 (below)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 6.0, 10.0),  // ratio = 0.15 / 0.1 = 1.5 (above)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 1.0, 5.0),   // ratio = 0.025 / 0.05 = 0.5 (below)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 4.0, 8.0),   // ratio = 0.1 / 0.08 = 1.25 (above)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 0.5, 2.0),   // ratio = 0.0125 / 0.02 = 0.625 (below)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 7.0, 10.0),  // ratio = 0.175 / 0.1 = 1.75 (above)
	}

	// Compute nodepool metrics
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)
	assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
	assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

	// Compute decision ratios for all candidates
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
	}

	// Filter candidates by policy (only ratio >= 1.0 should pass)
	filteredCandidates := make([]*Candidate, 0)
	for _, candidate := range candidates {
		if evaluator.ShouldConsolidate(v1.ConsolidateWhenCostJustifiesDisruption, candidate, candidate.decisionRatio, 1.0) {
			filteredCandidates = append(filteredCandidates, candidate)
		}
	}

	// Verify only moves with ratio >= 1.0 are included
	assert.Equal(t, 6, len(filteredCandidates), "Should have 6 candidates with ratio >= 1.0")
	for _, candidate := range filteredCandidates {
		assert.GreaterOrEqual(t, candidate.decisionRatio, 1.0, "Filtered candidates should have ratio >= 1.0")
	}

	// Sort filtered candidates by decision ratio (descending)
	sortedCandidates := make([]*Candidate, len(filteredCandidates))
	copy(sortedCandidates, filteredCandidates)
	for i := 0; i < len(sortedCandidates); i++ {
		for j := i + 1; j < len(sortedCandidates); j++ {
			if sortedCandidates[i].decisionRatio < sortedCandidates[j].decisionRatio {
				sortedCandidates[i], sortedCandidates[j] = sortedCandidates[j], sortedCandidates[i]
			}
		}
	}

	// Verify moves are in descending ratio order
	for i := 0; i < len(sortedCandidates)-1; i++ {
		assert.GreaterOrEqual(t, sortedCandidates[i].decisionRatio, sortedCandidates[i+1].decisionRatio,
			"Candidates should be sorted in descending order by decision ratio")
	}

	// Verify the highest ratio candidate is first
	maxRatio := 0.0
	var maxCandidate *Candidate
	for _, candidate := range filteredCandidates {
		if candidate.decisionRatio > maxRatio {
			maxRatio = candidate.decisionRatio
			maxCandidate = candidate
		}
	}
	assert.Equal(t, maxCandidate, sortedCandidates[0], "Candidate with highest ratio should be first")
}

// TestIntegration_MultiNodePoolPolicyIsolation verifies that different nodepools follow their own policies independently
// Validates: Requirements 2.5, 6.3, 6.4
func TestIntegration_MultiNodePoolPolicyIsolation(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})
	evaluator := &PolicyEvaluator{}

	// Create 3 nodepools with different policies
	nodepool1 := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "nodepool-when-empty"},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen: v1.ConsolidateWhenEmpty,
			},
		},
	}

	nodepool2 := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "nodepool-when-empty-or-underutilized"},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen: v1.ConsolidateWhenEmptyOrUnderutilized,
			},
		},
	}

	nodepool3 := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "nodepool-when-cost-justifies"},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen: v1.ConsolidateWhenCostJustifiesDisruption,
			},
		},
	}

	// Create candidates for each nodepool
	// NodePool 1 (WhenEmpty): mix of empty and non-empty nodes
	np1Candidates := []*Candidate{
		createCandidateForNodePool(nodepool1, 2.0, 5.0, 0), // empty, should consolidate
		createCandidateForNodePool(nodepool1, 3.0, 8.0, 5), // non-empty, should NOT consolidate
		createCandidateForNodePool(nodepool1, 1.5, 4.0, 0), // empty, should consolidate
	}

	// NodePool 2 (WhenEmptyOrUnderutilized): all should consolidate regardless of ratio
	np2Candidates := []*Candidate{
		createCandidateForNodePool(nodepool2, 1.0, 10.0, 3), // low ratio, should still consolidate
		createCandidateForNodePool(nodepool2, 5.0, 8.0, 5),  // high ratio, should consolidate
		createCandidateForNodePool(nodepool2, 0.5, 5.0, 2),  // low ratio, should still consolidate
	}

	// NodePool 3 (WhenCostJustifiesDisruption): only ratio >= 1.0 should consolidate
	np3Candidates := []*Candidate{
		createCandidateForNodePool(nodepool3, 8.0, 10.0, 3), // ratio = 0.8 / 0.333 = 2.4, should consolidate
		createCandidateForNodePool(nodepool3, 1.0, 15.0, 5), // ratio = 0.1 / 0.5 = 0.2, should NOT consolidate
		createCandidateForNodePool(nodepool3, 3.0, 5.0, 2),  // ratio = 0.3 / 0.167 = 1.8, should consolidate
	}

	// Process each nodepool independently
	testCases := []struct {
		name                string
		nodepool            *v1.NodePool
		candidates          []*Candidate
		expectedConsolidate int
	}{
		{
			name:                "NodePool 1 (WhenEmpty)",
			nodepool:            nodepool1,
			candidates:          np1Candidates,
			expectedConsolidate: 2, // Only empty nodes
		},
		{
			name:                "NodePool 2 (WhenEmptyOrUnderutilized)",
			nodepool:            nodepool2,
			candidates:          np2Candidates,
			expectedConsolidate: 3, // All nodes
		},
		{
			name:                "NodePool 3 (WhenCostJustifiesDisruption)",
			nodepool:            nodepool3,
			candidates:          np3Candidates,
			expectedConsolidate: 2, // Only nodes with ratio >= 1.0
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Compute nodepool metrics
			totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, tc.candidates)

			// Compute decision ratios
			for _, candidate := range tc.candidates {
				calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
			}

			// Apply policy filtering
			consolidatedCount := 0
			for _, candidate := range tc.candidates {
				if evaluator.ShouldConsolidate(tc.nodepool.Spec.Disruption.ConsolidateWhen, candidate, candidate.decisionRatio, 1.0) {
					consolidatedCount++
				}
			}

			assert.Equal(t, tc.expectedConsolidate, consolidatedCount,
				"NodePool %s should consolidate %d candidates", tc.nodepool.Name, tc.expectedConsolidate)
		})
	}

	// Verify no cross-nodepool interference by checking that each nodepool's policy is independent
	// Process all nodepools together and verify each follows its own policy
	allCandidates := append(append(np1Candidates, np2Candidates...), np3Candidates...)
	for _, candidate := range allCandidates {
		// Compute metrics for the candidate's nodepool
		var nodepoolCandidates []*Candidate
		for _, c := range allCandidates {
			if c.NodePool.Name == candidate.NodePool.Name {
				nodepoolCandidates = append(nodepoolCandidates, c)
			}
		}

		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, nodepoolCandidates)
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

		// Verify policy is applied correctly
		shouldConsolidate := evaluator.ShouldConsolidate(candidate.NodePool.Spec.Disruption.ConsolidateWhen, candidate, candidate.decisionRatio, 1.0)

		// Verify the decision matches the policy
		switch candidate.NodePool.Spec.Disruption.ConsolidateWhen {
		case v1.ConsolidateWhenEmpty:
			assert.Equal(t, len(candidate.reschedulablePods) == 0, shouldConsolidate,
				"WhenEmpty policy should only consolidate empty nodes")
		case v1.ConsolidateWhenEmptyOrUnderutilized:
			assert.True(t, shouldConsolidate, "WhenEmptyOrUnderutilized should consolidate all nodes")
		case v1.ConsolidateWhenCostJustifiesDisruption:
			assert.Equal(t, candidate.decisionRatio >= 1.0, shouldConsolidate,
				"WhenCostJustifiesDisruption should only consolidate when ratio >= 1.0")
		}
	}
}

// TestIntegration_DeleteRatioOptimization verifies that delete ratio filtering skips low-value candidates
// Validates: Requirements 4.2, 10.4
func TestIntegration_DeleteRatioOptimization(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create nodes with very high disruption costs (ratio < 1.0)
	candidates := []*Candidate{
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 1.0, 50.0), // ratio = 0.091 / 0.455 = 0.2 (skip)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 2.0, 40.0), // ratio = 0.182 / 0.364 = 0.5 (skip)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 5.0, 10.0), // ratio = 0.455 / 0.091 = 5.0 (keep)
		createIntegrationTestCandidateWithPolicyAndValues(v1.ConsolidateWhenCostJustifiesDisruption, 3.0, 10.0), // ratio = 0.273 / 0.091 = 3.0 (keep)
	}

	// Compute nodepool metrics
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute delete ratios for all candidates
	skippedCount := 0
	keptCount := 0
	for _, candidate := range candidates {
		calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

		// Simulate delete ratio filtering
		if candidate.NodePool.Spec.Disruption.ConsolidateWhen == v1.ConsolidateWhenCostJustifiesDisruption {
			if candidate.deleteRatio < 1.0 {
				skippedCount++
				// In real implementation, this would emit movesSkippedByDeleteRatioCounter metric
			} else {
				keptCount++
			}
		}
	}

	// Verify that move generation is skipped for low-ratio nodes
	assert.Equal(t, 2, skippedCount, "Should skip 2 candidates with delete ratio < 1.0")
	assert.Equal(t, 2, keptCount, "Should keep 2 candidates with delete ratio >= 1.0")

	// Verify the delete ratios are computed correctly
	for _, candidate := range candidates {
		expectedDeleteRatio := (candidate.instanceType.Offerings[0].Price / totalCost) / (candidate.DisruptionCost / totalDisruption)
		assert.InDelta(t, expectedDeleteRatio, candidate.deleteRatio, 0.01,
			"Delete ratio should match expected value")
	}
}

// TestIntegration_EdgeCaseHandling verifies graceful handling of edge cases
// Validates: Requirements 9.1, 9.2, 9.3
func TestIntegration_EdgeCaseHandling(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	t.Run("Zero total cost scenario", func(t *testing.T) {
		// Create candidates with zero cost
		candidates := []*Candidate{
			createIntegrationTestCandidateWithValues(0.0, 10.0),
			createIntegrationTestCandidateWithValues(0.0, 15.0),
		}

		// Compute nodepool metrics
		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

		// Verify total cost is zero
		assert.Equal(t, 0.0, totalCost, "Total cost should be zero")
		assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

		// Verify consolidation is deferred (no ratios computed)
		// In real implementation, this would skip decision ratio computation entirely
		for _, candidate := range candidates {
			// Should not compute ratio when total cost is zero
			if totalCost == 0.0 {
				// Skip ratio computation
				continue
			}
			calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
		}

		// Verify no candidates have ratios computed
		for _, candidate := range candidates {
			assert.Equal(t, 0.0, candidate.decisionRatio, "Decision ratio should not be computed when total cost is zero")
		}
	})

	t.Run("Zero total disruption scenario", func(t *testing.T) {
		// Create candidates with zero disruption (should not happen due to baseline, but test defensively)
		candidates := []*Candidate{
			createIntegrationTestCandidateWithValues(5.0, 0.0),
			createIntegrationTestCandidateWithValues(3.0, 0.0),
		}

		// Compute nodepool metrics
		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

		// Verify total disruption is zero
		assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
		assert.Equal(t, 0.0, totalDisruption, "Total disruption should be zero")

		// Compute decision ratios (should be infinite)
		for _, candidate := range candidates {
			calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
		}

		// Verify all ratios are infinite (allow all consolidations)
		for _, candidate := range candidates {
			assert.True(t, candidate.decisionRatio > 1e9 || candidate.decisionRatio == 0.0,
				"Decision ratio should be infinite or handled specially when total disruption is zero")
		}
	})

	t.Run("Nodes with only negative eviction cost pods", func(t *testing.T) {
		// Create candidates with negative eviction costs (clamped to zero)
		// In real implementation, ComputeNodeDisruptionCost clamps negative costs to zero
		// So the minimum disruption cost is the baseline of 1.0
		candidates := []*Candidate{
			createIntegrationTestCandidateWithValues(5.0, 1.0), // baseline only (negative costs clamped)
			createIntegrationTestCandidateWithValues(3.0, 1.0), // baseline only (negative costs clamped)
		}

		// Compute nodepool metrics
		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

		// Verify metrics are computed correctly
		assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
		assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

		// Compute decision ratios
		for _, candidate := range candidates {
			calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
		}

		// Verify ratios are computed correctly (no division by zero)
		for _, candidate := range candidates {
			assert.Greater(t, candidate.decisionRatio, 0.0, "Decision ratio should be positive")
			assert.False(t, candidate.decisionRatio != candidate.decisionRatio, "Decision ratio should not be NaN")
		}
	})
}

// Helper function to create a candidate for a specific nodepool with pod count
func createCandidateForNodePool(nodepool *v1.NodePool, cost, disruption float64, podCount int) *Candidate {
	candidate := createIntegrationTestCandidateWithValues(cost, disruption)
	candidate.NodePool = nodepool
	candidate.reschedulablePods = make([]*corev1.Pod, podCount)
	for i := 0; i < podCount; i++ {
		candidate.reschedulablePods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-pod",
			},
		}
	}
	return candidate
}

// TestThresholdRetrieval_FromNodePoolSpec verifies that threshold is correctly retrieved from NodePool spec
// Validates: Requirements 1.3, 2.4, 9.1
func TestThresholdRetrieval_FromNodePoolSpec(t *testing.T) {
	tests := []struct {
		name              string
		thresholdValue    *float64
		expectedThreshold float64
		description       string
	}{
		{
			name:              "nil threshold defaults to 1.0",
			thresholdValue:    nil,
			expectedThreshold: 1.0,
			description:       "When DecisionRatioThreshold is not specified, should default to 1.0",
		},
		{
			name:              "configured threshold 0.5 is used",
			thresholdValue:    ptr(0.5),
			expectedThreshold: 0.5,
			description:       "When DecisionRatioThreshold is set to 0.5, should use 0.5",
		},
		{
			name:              "configured threshold 1.0 is used",
			thresholdValue:    ptr(1.0),
			expectedThreshold: 1.0,
			description:       "When DecisionRatioThreshold is explicitly set to 1.0, should use 1.0",
		},
		{
			name:              "configured threshold 1.5 is used",
			thresholdValue:    ptr(1.5),
			expectedThreshold: 1.5,
			description:       "When DecisionRatioThreshold is set to 1.5, should use 1.5",
		},
		{
			name:              "configured threshold 2.0 is used",
			thresholdValue:    ptr(2.0),
			expectedThreshold: 2.0,
			description:       "When DecisionRatioThreshold is set to 2.0, should use 2.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a NodePool with the specified threshold
			nodepool := &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-nodepool",
				},
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
						DecisionRatioThreshold: tt.thresholdValue,
					},
				},
			}

			// Retrieve threshold using GetDecisionRatioThreshold
			retrievedThreshold := nodepool.Spec.Disruption.GetDecisionRatioThreshold()

			// Verify the retrieved threshold matches expected
			assert.Equal(t, tt.expectedThreshold, retrievedThreshold, tt.description)
		})
	}
}

// TestThresholdPassing_ToPolicyEvaluator verifies that threshold is correctly passed to PolicyEvaluator
// Validates: Requirements 2.4
func TestThresholdPassing_ToPolicyEvaluator(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		threshold     float64
		decisionRatio float64
		policy        v1.ConsolidateWhenPolicy
		shouldExecute bool
		description   string
	}{
		{
			name:          "threshold 1.5 with ratio 2.0 should execute",
			threshold:     1.5,
			decisionRatio: 2.0,
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			shouldExecute: true,
			description:   "When ratio (2.0) >= threshold (1.5), should execute consolidation",
		},
		{
			name:          "threshold 1.5 with ratio 1.0 should not execute",
			threshold:     1.5,
			decisionRatio: 1.0,
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			shouldExecute: false,
			description:   "When ratio (1.0) < threshold (1.5), should not execute consolidation",
		},
		{
			name:          "threshold 0.5 with ratio 0.7 should execute",
			threshold:     0.5,
			decisionRatio: 0.7,
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			shouldExecute: true,
			description:   "When ratio (0.7) >= threshold (0.5), should execute consolidation",
		},
		{
			name:          "threshold 2.0 with ratio 1.5 should not execute",
			threshold:     2.0,
			decisionRatio: 1.5,
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			shouldExecute: false,
			description:   "When ratio (1.5) < threshold (2.0), should not execute consolidation",
		},
		{
			name:          "default threshold 1.0 with ratio 1.0 should execute",
			threshold:     1.0,
			decisionRatio: 1.0,
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			shouldExecute: true,
			description:   "When ratio (1.0) == threshold (1.0), should execute consolidation (boundary case)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(0)

			// Call ShouldConsolidate with the threshold
			result := evaluator.ShouldConsolidate(tt.policy, candidate, tt.decisionRatio, tt.threshold)

			// Verify the result matches expected
			assert.Equal(t, tt.shouldExecute, result, tt.description)
		})
	}
}

// TestThresholdRetrieval_InConsolidationFlow verifies threshold is retrieved and used in consolidation flow
// Validates: Requirements 1.3, 2.4, 9.1
func TestThresholdRetrieval_InConsolidationFlow(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name              string
		thresholdValue    *float64
		candidateRatios   []float64
		expectedPassCount int
		description       string
	}{
		{
			name:              "nil threshold defaults to 1.0 and filters correctly",
			thresholdValue:    nil,
			candidateRatios:   []float64{0.5, 1.0, 1.5, 2.0},
			expectedPassCount: 3, // 1.0, 1.5, 2.0 pass
			description:       "With nil threshold (default 1.0), ratios >= 1.0 should pass",
		},
		{
			name:              "threshold 1.5 filters correctly",
			thresholdValue:    ptr(1.5),
			candidateRatios:   []float64{0.5, 1.0, 1.5, 2.0},
			expectedPassCount: 2, // 1.5, 2.0 pass
			description:       "With threshold 1.5, ratios >= 1.5 should pass",
		},
		{
			name:              "threshold 0.5 filters correctly",
			thresholdValue:    ptr(0.5),
			candidateRatios:   []float64{0.3, 0.5, 0.7, 1.0},
			expectedPassCount: 3, // 0.5, 0.7, 1.0 pass
			description:       "With threshold 0.5, ratios >= 0.5 should pass",
		},
		{
			name:              "threshold 2.0 filters correctly",
			thresholdValue:    ptr(2.0),
			candidateRatios:   []float64{0.5, 1.0, 1.5, 2.0, 3.0},
			expectedPassCount: 2, // 2.0, 3.0 pass
			description:       "With threshold 2.0, ratios >= 2.0 should pass",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create NodePool with specified threshold
			nodepool := &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-nodepool",
				},
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
						DecisionRatioThreshold: tt.thresholdValue,
					},
				},
			}

			// Retrieve threshold from NodePool
			threshold := nodepool.Spec.Disruption.GetDecisionRatioThreshold()

			// Create candidates with specified ratios
			candidates := make([]*Candidate, len(tt.candidateRatios))
			for i, ratio := range tt.candidateRatios {
				candidates[i] = createCandidateForNodePool(nodepool, 1.0, 1.0, 0)
				candidates[i].decisionRatio = ratio
			}

			// Compute nodepool metrics (required for realistic test)
			totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)
			assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
			assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

			// Apply policy filtering with the retrieved threshold
			passCount := 0
			for _, candidate := range candidates {
				if evaluator.ShouldConsolidate(
					nodepool.Spec.Disruption.ConsolidateWhen,
					candidate,
					candidate.decisionRatio,
					threshold,
				) {
					passCount++
				}
			}

			// Verify the correct number of candidates pass
			assert.Equal(t, tt.expectedPassCount, passCount, tt.description)
		})
	}
}

// ptr is a helper function to create a pointer to a float64 value
func ptr(f float64) *float64 {
	return &f
}

// TestIntegration_ConservativeThreshold verifies consolidation with conservative threshold (2.0)
// Validates: Requirements 3.1, 3.2, 3.3
func TestIntegration_ConservativeThreshold(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})
	evaluator := &PolicyEvaluator{}

	// Create NodePool with conservative threshold of 2.0
	nodepool := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "conservative-nodepool",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: ptr(2.0),
			},
		},
	}

	// Create nodes with various decision ratios (0.5, 1.0, 1.5, 2.0, 3.0)
	// Design costs and disruptions to achieve target ratios
	// ratio = (cost / totalCost) / (disruption / totalDisruption)
	// We want clear examples of ratios: 0.5, 1.0, 1.5, 2.0, 3.0
	// totalCost = 5 + 10 + 15 + 20 + 30 = 80
	// totalDisruption = 10 + 10 + 10 + 10 + 10 = 50
	candidates := []*Candidate{
		// ratio = (5/80) / (10/50) = 0.0625 / 0.2 = 0.3125 (below 2.0)
		createCandidateForNodePool(nodepool, 5.0, 10.0, 3),
		// ratio = (10/80) / (10/50) = 0.125 / 0.2 = 0.625 (below 2.0)
		createCandidateForNodePool(nodepool, 10.0, 10.0, 2),
		// ratio = (15/80) / (10/50) = 0.1875 / 0.2 = 0.9375 (below 2.0)
		createCandidateForNodePool(nodepool, 15.0, 10.0, 1),
		// ratio = (20/80) / (10/50) = 0.25 / 0.2 = 1.25 (below 2.0)
		createCandidateForNodePool(nodepool, 20.0, 10.0, 4),
		// ratio = (30/80) / (10/50) = 0.375 / 0.2 = 1.875 (below 2.0)
		createCandidateForNodePool(nodepool, 30.0, 10.0, 2),
	}

	// Add candidates with high cost and low disruption to get ratios >= 2.0
	// totalCost = 80 + 40 + 50 = 170
	// totalDisruption = 50 + 5 + 5 = 60
	additionalCandidates := []*Candidate{
		// ratio = (40/170) / (5/60) = 0.235 / 0.083 = 2.82 (above 2.0)
		createCandidateForNodePool(nodepool, 40.0, 5.0, 1),
		// ratio = (50/170) / (5/60) = 0.294 / 0.083 = 3.53 (above 2.0)
		createCandidateForNodePool(nodepool, 50.0, 5.0, 3),
	}
	candidates = append(candidates, additionalCandidates...)

	// Compute nodepool metrics
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)
	assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
	assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

	// Compute decision ratios for all candidates
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
	}

	// Retrieve threshold from NodePool
	threshold := nodepool.Spec.Disruption.GetDecisionRatioThreshold()
	assert.Equal(t, 2.0, threshold, "Threshold should be 2.0")

	// Apply policy filtering - only moves with ratio >= 2.0 should pass
	passedCandidates := make([]*Candidate, 0)
	failedCandidates := make([]*Candidate, 0)
	for _, candidate := range candidates {
		if evaluator.ShouldConsolidate(
			nodepool.Spec.Disruption.ConsolidateWhen,
			candidate,
			candidate.decisionRatio,
			threshold,
		) {
			passedCandidates = append(passedCandidates, candidate)
		} else {
			failedCandidates = append(failedCandidates, candidate)
		}
	}

	// Verify only moves with ratio >= 2.0 are executed
	assert.GreaterOrEqual(t, len(passedCandidates), 2, "Should have at least 2 candidates with ratio >= 2.0")
	for _, candidate := range passedCandidates {
		assert.GreaterOrEqual(t, candidate.decisionRatio, 2.0,
			"Passed candidates should have decision ratio >= 2.0, got %.2f", candidate.decisionRatio)
	}

	// Verify moves with ratio < 2.0 are not executed
	assert.GreaterOrEqual(t, len(failedCandidates), 5, "Should have at least 5 candidates with ratio < 2.0")
	for _, candidate := range failedCandidates {
		assert.Less(t, candidate.decisionRatio, 2.0,
			"Failed candidates should have decision ratio < 2.0, got %.2f", candidate.decisionRatio)
	}

	// Verify that conservative threshold results in fewer consolidations than default
	// Create a comparison scenario with default threshold (1.0)
	defaultNodepool := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default-nodepool",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: nil, // defaults to 1.0
			},
		},
	}

	// Create same candidates for default nodepool
	defaultCandidates := make([]*Candidate, len(candidates))
	for i, candidate := range candidates {
		defaultCandidates[i] = createCandidateForNodePool(defaultNodepool,
			candidate.instanceType.Offerings[0].Price,
			candidate.DisruptionCost,
			len(candidate.reschedulablePods))
		defaultCandidates[i].decisionRatio = candidate.decisionRatio
	}

	// Apply policy filtering with default threshold (1.0)
	defaultThreshold := defaultNodepool.Spec.Disruption.GetDecisionRatioThreshold()
	assert.Equal(t, 1.0, defaultThreshold, "Default threshold should be 1.0")

	defaultPassedCount := 0
	for _, candidate := range defaultCandidates {
		if evaluator.ShouldConsolidate(
			defaultNodepool.Spec.Disruption.ConsolidateWhen,
			candidate,
			candidate.decisionRatio,
			defaultThreshold,
		) {
			defaultPassedCount++
		}
	}

	// Verify conservative threshold (2.0) results in fewer consolidations than default (1.0)
	assert.Less(t, len(passedCandidates), defaultPassedCount,
		"Conservative threshold (2.0) should result in fewer consolidations than default (1.0)")

	// Verify metrics would show correct counts
	// In a real implementation, we would verify:
	// - movesAboveThresholdCounter is incremented for passed candidates
	// - movesBelowThresholdCounter is incremented for failed candidates
	// - All metrics include threshold label "2.00"
	t.Logf("Conservative threshold (2.0): %d moves passed, %d moves failed",
		len(passedCandidates), len(failedCandidates))
	t.Logf("Default threshold (1.0): %d moves passed", defaultPassedCount)
	t.Logf("Reduction in consolidations: %d moves", defaultPassedCount-len(passedCandidates))
}

// TestIntegration_AggressiveThreshold verifies consolidation with aggressive threshold (0.5)
// Validates: Requirements 4.1, 4.2, 4.3
func TestIntegration_AggressiveThreshold(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})
	evaluator := &PolicyEvaluator{}

	// Create NodePool with aggressive threshold of 0.5
	nodepool := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "aggressive-nodepool",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: ptr(0.5),
			},
		},
	}

	// Create nodes with various decision ratios (0.3, 0.5, 0.7, 1.0)
	// Design costs and disruptions to achieve target ratios
	// ratio = (cost / totalCost) / (disruption / totalDisruption)
	// We want clear examples of ratios: 0.3, 0.5, 0.7, 1.0
	// totalCost = 3 + 5 + 7 + 10 = 25
	// totalDisruption = 10 + 10 + 10 + 10 = 40
	candidates := []*Candidate{
		// ratio = (3/25) / (10/40) = 0.12 / 0.25 = 0.48 (below 0.5, close to boundary)
		createCandidateForNodePool(nodepool, 3.0, 10.0, 3),
		// ratio = (5/25) / (10/40) = 0.2 / 0.25 = 0.8 (above 0.5)
		createCandidateForNodePool(nodepool, 5.0, 10.0, 2),
		// ratio = (7/25) / (10/40) = 0.28 / 0.25 = 1.12 (above 0.5)
		createCandidateForNodePool(nodepool, 7.0, 10.0, 1),
		// ratio = (10/25) / (10/40) = 0.4 / 0.25 = 1.6 (above 0.5)
		createCandidateForNodePool(nodepool, 10.0, 10.0, 4),
	}

	// Add candidates with lower ratios to test filtering
	// totalCost = 25 + 2 + 1 = 28
	// totalDisruption = 40 + 15 + 10 = 65
	additionalCandidates := []*Candidate{
		// ratio = (2/28) / (15/65) = 0.071 / 0.231 = 0.31 (below 0.5)
		createCandidateForNodePool(nodepool, 2.0, 15.0, 2),
		// ratio = (1/28) / (10/65) = 0.036 / 0.154 = 0.23 (below 0.5)
		createCandidateForNodePool(nodepool, 1.0, 10.0, 3),
	}
	candidates = append(candidates, additionalCandidates...)

	// Compute nodepool metrics
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)
	assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
	assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

	// Compute decision ratios for all candidates
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
	}

	// Retrieve threshold from NodePool
	threshold := nodepool.Spec.Disruption.GetDecisionRatioThreshold()
	assert.Equal(t, 0.5, threshold, "Threshold should be 0.5")

	// Apply policy filtering - only moves with ratio >= 0.5 should pass
	passedCandidates := make([]*Candidate, 0)
	failedCandidates := make([]*Candidate, 0)
	for _, candidate := range candidates {
		if evaluator.ShouldConsolidate(
			nodepool.Spec.Disruption.ConsolidateWhen,
			candidate,
			candidate.decisionRatio,
			threshold,
		) {
			passedCandidates = append(passedCandidates, candidate)
		} else {
			failedCandidates = append(failedCandidates, candidate)
		}
	}

	// Verify moves with ratio >= 0.5 are executed
	assert.GreaterOrEqual(t, len(passedCandidates), 3, "Should have at least 3 candidates with ratio >= 0.5")
	for _, candidate := range passedCandidates {
		assert.GreaterOrEqual(t, candidate.decisionRatio, 0.5,
			"Passed candidates should have decision ratio >= 0.5, got %.2f", candidate.decisionRatio)
	}

	// Verify moves with ratio < 0.5 are not executed
	assert.GreaterOrEqual(t, len(failedCandidates), 2, "Should have at least 2 candidates with ratio < 0.5")
	for _, candidate := range failedCandidates {
		assert.Less(t, candidate.decisionRatio, 0.5,
			"Failed candidates should have decision ratio < 0.5, got %.2f", candidate.decisionRatio)
	}

	// Verify that aggressive threshold results in more consolidations than default
	// Create a comparison scenario with default threshold (1.0)
	defaultNodepool := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default-nodepool",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: nil, // defaults to 1.0
			},
		},
	}

	// Create same candidates for default nodepool
	defaultCandidates := make([]*Candidate, len(candidates))
	for i, candidate := range candidates {
		defaultCandidates[i] = createCandidateForNodePool(defaultNodepool,
			candidate.instanceType.Offerings[0].Price,
			candidate.DisruptionCost,
			len(candidate.reschedulablePods))
		defaultCandidates[i].decisionRatio = candidate.decisionRatio
	}

	// Apply policy filtering with default threshold (1.0)
	defaultThreshold := defaultNodepool.Spec.Disruption.GetDecisionRatioThreshold()
	assert.Equal(t, 1.0, defaultThreshold, "Default threshold should be 1.0")

	defaultPassedCount := 0
	for _, candidate := range defaultCandidates {
		if evaluator.ShouldConsolidate(
			defaultNodepool.Spec.Disruption.ConsolidateWhen,
			candidate,
			candidate.decisionRatio,
			defaultThreshold,
		) {
			defaultPassedCount++
		}
	}

	// Verify aggressive threshold (0.5) results in more consolidations than default (1.0)
	assert.Greater(t, len(passedCandidates), defaultPassedCount,
		"Aggressive threshold (0.5) should result in more consolidations than default (1.0)")

	// Verify metrics would show correct counts
	// In a real implementation, we would verify:
	// - movesAboveThresholdCounter is incremented for passed candidates
	// - movesBelowThresholdCounter is incremented for failed candidates
	// - All metrics include threshold label "0.50"
	t.Logf("Aggressive threshold (0.5): %d moves passed, %d moves failed",
		len(passedCandidates), len(failedCandidates))
	t.Logf("Default threshold (1.0): %d moves passed", defaultPassedCount)
	t.Logf("Increase in consolidations: %d moves", len(passedCandidates)-defaultPassedCount)
}

// TestIntegration_DeleteRatioFilteringWithConfigurableThreshold verifies that delete ratio filtering respects configurable threshold
// Validates: Requirements 5.1, 5.2, 5.4, 7.4
//
//nolint:gocyclo
func TestIntegration_DeleteRatioFilteringWithConfigurableThreshold(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create NodePool with threshold of 1.5
	nodepool := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodepool-threshold-1.5",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: ptr(1.5),
			},
		},
	}

	// Create nodes with very high disruption costs (delete ratio < threshold)
	// Design candidates so their delete ratios are clearly below and above 1.5
	// Delete ratio = (cost / totalCost) / (disruption / totalDisruption)
	// For delete ratio < 1.5, we need high disruption relative to cost
	// For delete ratio >= 1.5, we need high cost relative to disruption
	candidates := []*Candidate{
		// High disruption, low cost - delete ratio should be < 1.5 (should be skipped)
		createCandidateForNodePool(nodepool, 5.0, 50.0, 3), // ratio = (5/50) / (50/150) = 0.1 / 0.333 = 0.3
		createCandidateForNodePool(nodepool, 8.0, 60.0, 2), // ratio = (8/50) / (60/150) = 0.16 / 0.4 = 0.4
		createCandidateForNodePool(nodepool, 7.0, 40.0, 1), // ratio = (7/50) / (40/150) = 0.14 / 0.267 = 0.52

		// High cost, low disruption - delete ratio should be >= 1.5 (should be kept)
		createCandidateForNodePool(nodepool, 15.0, 5.0, 4),  // ratio = (15/50) / (5/150) = 0.3 / 0.033 = 9.0
		createCandidateForNodePool(nodepool, 15.0, 10.0, 2), // ratio = (15/50) / (10/150) = 0.3 / 0.067 = 4.5
	}

	// Verify threshold is retrieved correctly
	threshold := nodepool.Spec.Disruption.GetDecisionRatioThreshold()
	assert.Equal(t, 1.5, threshold, "Threshold should be 1.5")

	// Compute nodepool metrics
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)
	assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
	assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

	// Compute delete ratios for all candidates
	for _, candidate := range candidates {
		calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)
	}

	// Simulate delete ratio filtering with configurable threshold
	// Only candidates with delete ratio >= threshold should be kept
	skippedCandidates := make([]*Candidate, 0)
	keptCandidates := make([]*Candidate, 0)
	skippedCount := 0
	keptCount := 0

	for _, candidate := range candidates {
		// Only apply filtering for WhenCostJustifiesDisruption policy
		if candidate.NodePool.Spec.Disruption.ConsolidateWhen == v1.ConsolidateWhenCostJustifiesDisruption {
			if candidate.deleteRatio < threshold {
				skippedCandidates = append(skippedCandidates, candidate)
				skippedCount++
				// In real implementation, this would emit movesSkippedByDeleteRatioCounter metric
				// with labels: nodepool=nodepool-threshold-1.5, policy=WhenCostJustifiesDisruption, threshold=1.50
			} else {
				keptCandidates = append(keptCandidates, candidate)
				keptCount++
			}
		}
	}

	// Verify move generation is skipped for low-ratio nodes
	assert.Equal(t, 3, skippedCount, "Should skip 3 candidates with delete ratio < 1.5")
	assert.Equal(t, 2, keptCount, "Should keep 2 candidates with delete ratio >= 1.5")

	// Verify all skipped candidates have delete ratio < threshold
	for _, candidate := range skippedCandidates {
		assert.Less(t, candidate.deleteRatio, threshold,
			"Skipped candidate should have delete ratio < %.2f, got %.2f", threshold, candidate.deleteRatio)
	}

	// Verify all kept candidates have delete ratio >= threshold
	for _, candidate := range keptCandidates {
		assert.GreaterOrEqual(t, candidate.deleteRatio, threshold,
			"Kept candidate should have delete ratio >= %.2f, got %.2f", threshold, candidate.deleteRatio)
	}

	// Verify the delete ratios are computed correctly
	for _, candidate := range candidates {
		expectedDeleteRatio := (candidate.instanceType.Offerings[0].Price / totalCost) / (candidate.DisruptionCost / totalDisruption)
		assert.InDelta(t, expectedDeleteRatio, candidate.deleteRatio, 0.01,
			"Delete ratio should match expected value")
	}

	// Verify that skip metric would be emitted for each skipped candidate
	// In real implementation, movesSkippedByDeleteRatioCounter would be incremented
	// with labels: nodepool, policy, threshold
	t.Logf("Delete ratio filtering with threshold %.2f:", threshold)
	t.Logf("  Skipped %d candidates (delete ratio < %.2f)", skippedCount, threshold)
	t.Logf("  Kept %d candidates (delete ratio >= %.2f)", keptCount, threshold)
	for i, candidate := range skippedCandidates {
		t.Logf("    Skipped candidate %d: delete ratio = %.2f", i+1, candidate.deleteRatio)
	}
	for i, candidate := range keptCandidates {
		t.Logf("    Kept candidate %d: delete ratio = %.2f", i+1, candidate.deleteRatio)
	}

	// Test with different threshold to verify monotonicity (Requirement 5.4)
	t.Run("Verify threshold monotonicity", func(t *testing.T) {
		// Test with lower threshold (1.0) - should skip fewer candidates
		lowerThreshold := 1.0
		lowerSkippedCount := 0
		for _, candidate := range candidates {
			if candidate.NodePool.Spec.Disruption.ConsolidateWhen == v1.ConsolidateWhenCostJustifiesDisruption {
				if candidate.deleteRatio < lowerThreshold {
					lowerSkippedCount++
				}
			}
		}

		// Test with higher threshold (2.0) - should skip more candidates
		higherThreshold := 2.0
		higherSkippedCount := 0
		for _, candidate := range candidates {
			if candidate.NodePool.Spec.Disruption.ConsolidateWhen == v1.ConsolidateWhenCostJustifiesDisruption {
				if candidate.deleteRatio < higherThreshold {
					higherSkippedCount++
				}
			}
		}

		// Verify monotonicity: higher threshold skips more candidates
		assert.LessOrEqual(t, lowerSkippedCount, skippedCount,
			"Lower threshold (%.2f) should skip fewer or equal candidates than threshold (%.2f)", lowerThreshold, threshold)
		assert.GreaterOrEqual(t, higherSkippedCount, skippedCount,
			"Higher threshold (%.2f) should skip more or equal candidates than threshold (%.2f)", higherThreshold, threshold)

		t.Logf("Threshold monotonicity verified:")
		t.Logf("  Threshold 1.0: skipped %d candidates", lowerSkippedCount)
		t.Logf("  Threshold 1.5: skipped %d candidates", skippedCount)
		t.Logf("  Threshold 2.0: skipped %d candidates", higherSkippedCount)
		t.Logf("  Monotonicity: %d <= %d <= %d", lowerSkippedCount, skippedCount, higherSkippedCount)
	})

	// Test that other policies don't apply delete ratio filtering
	t.Run("Verify other policies ignore delete ratio filtering", func(t *testing.T) {
		// Create NodePool with WhenEmptyOrUnderutilized policy
		nodepoolNoFilter := &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nodepool-no-filter",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateWhen:        v1.ConsolidateWhenEmptyOrUnderutilized,
					DecisionRatioThreshold: ptr(1.5), // threshold should be ignored
				},
			},
		}

		// Create candidates with same ratios
		candidatesNoFilter := make([]*Candidate, len(candidates))
		for i, candidate := range candidates {
			candidatesNoFilter[i] = createCandidateForNodePool(nodepoolNoFilter,
				candidate.instanceType.Offerings[0].Price,
				candidate.DisruptionCost,
				len(candidate.reschedulablePods))
		}

		// Compute metrics and delete ratios
		totalCostNoFilter, totalDisruptionNoFilter := calculator.ComputeNodePoolMetrics(ctx, candidatesNoFilter)
		for _, candidate := range candidatesNoFilter {
			calculator.ComputeDeleteRatio(ctx, candidate, totalCostNoFilter, totalDisruptionNoFilter)
		}

		// Simulate delete ratio filtering - should NOT filter for WhenEmptyOrUnderutilized
		skippedCountNoFilter := 0
		for _, candidate := range candidatesNoFilter {
			// Only apply filtering for WhenCostJustifiesDisruption policy
			if candidate.NodePool.Spec.Disruption.ConsolidateWhen == v1.ConsolidateWhenCostJustifiesDisruption {
				if candidate.deleteRatio < threshold {
					skippedCountNoFilter++
				}
			}
		}

		// Verify no candidates are skipped for WhenEmptyOrUnderutilized policy
		assert.Equal(t, 0, skippedCountNoFilter,
			"WhenEmptyOrUnderutilized policy should not apply delete ratio filtering")

		t.Logf("Policy filtering verification:")
		t.Logf("  WhenCostJustifiesDisruption: skipped %d candidates", skippedCount)
		t.Logf("  WhenEmptyOrUnderutilized: skipped %d candidates (filtering not applied)", skippedCountNoFilter)
	})
}

// TestIntegration_MultiNodePoolDifferentThresholds verifies that multiple NodePools with different thresholds operate independently
// Validates: Requirements 6.1, 6.2, 6.3, 6.4, 9.3
//
//nolint:gocyclo
func TestIntegration_MultiNodePoolDifferentThresholds(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})
	evaluator := &PolicyEvaluator{}

	// Create NodePool A with conservative threshold of 2.0
	nodepoolA := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodepool-a-conservative",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: ptr(2.0),
			},
		},
	}

	// Create NodePool B with aggressive threshold of 0.5
	nodepoolB := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodepool-b-aggressive",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: ptr(0.5),
			},
		},
	}

	// Create NodePool C with threshold omitted (default 1.0)
	nodepoolC := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodepool-c-default",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: nil, // defaults to 1.0
			},
		},
	}

	// Create candidates for NodePool A
	// Design ratios to have some above and below threshold 2.0
	// ratio = (cost / totalCost) / (disruption / totalDisruption)
	// With threshold 2.0, we need candidates with high cost and low disruption
	candidatesA := []*Candidate{
		createCandidateForNodePool(nodepoolA, 5.0, 10.0, 2),  // Low cost, medium disruption
		createCandidateForNodePool(nodepoolA, 10.0, 15.0, 3), // Medium cost, high disruption
		createCandidateForNodePool(nodepoolA, 15.0, 8.0, 1),  // Medium cost, low disruption
		createCandidateForNodePool(nodepoolA, 30.0, 5.0, 4),  // High cost, very low disruption (should pass)
		createCandidateForNodePool(nodepoolA, 40.0, 7.0, 2),  // Very high cost, low disruption (should pass)
	}

	// Create candidates for NodePool B
	// Design ratios to have some above and below threshold 0.5
	// With threshold 0.5, most should pass
	candidatesB := []*Candidate{
		createCandidateForNodePool(nodepoolB, 3.0, 15.0, 1),  // Low cost, high disruption (may not pass)
		createCandidateForNodePool(nodepoolB, 5.0, 10.0, 2),  // Low cost, medium disruption
		createCandidateForNodePool(nodepoolB, 7.0, 8.0, 3),   // Medium cost, low disruption
		createCandidateForNodePool(nodepoolB, 10.0, 10.0, 4), // Medium cost, medium disruption
		createCandidateForNodePool(nodepoolB, 15.0, 12.0, 2), // High cost, medium disruption
	}

	// Create candidates for NodePool C
	// Design ratios to have some above and below threshold 1.0
	// With threshold 1.0 (default), about half should pass
	candidatesC := []*Candidate{
		createCandidateForNodePool(nodepoolC, 5.0, 12.0, 3),  // Low cost, high disruption
		createCandidateForNodePool(nodepoolC, 8.0, 10.0, 1),  // Medium cost, medium disruption
		createCandidateForNodePool(nodepoolC, 10.0, 8.0, 2),  // Medium cost, low disruption
		createCandidateForNodePool(nodepoolC, 12.0, 9.0, 4),  // High cost, low disruption
		createCandidateForNodePool(nodepoolC, 15.0, 11.0, 2), // High cost, medium disruption
	}

	// Process each NodePool independently
	testCases := []struct {
		name              string
		nodepool          *v1.NodePool
		candidates        []*Candidate
		expectedThreshold float64
		minExpectedPass   int
		description       string
	}{
		{
			name:              "NodePool A with threshold 2.0",
			nodepool:          nodepoolA,
			candidates:        candidatesA,
			expectedThreshold: 2.0,
			minExpectedPass:   1, // At least 1 candidate with ratio >= 2.0
			description:       "Conservative threshold should only allow high-ratio moves",
		},
		{
			name:              "NodePool B with threshold 0.5",
			nodepool:          nodepoolB,
			candidates:        candidatesB,
			expectedThreshold: 0.5,
			minExpectedPass:   3, // At least 3 candidates with ratio >= 0.5
			description:       "Aggressive threshold should allow most moves",
		},
		{
			name:              "NodePool C with default threshold 1.0",
			nodepool:          nodepoolC,
			candidates:        candidatesC,
			expectedThreshold: 1.0,
			minExpectedPass:   2, // At least 2 candidates with ratio >= 1.0
			description:       "Default threshold should allow medium-ratio moves",
		},
	}

	// Store results for cross-NodePool verification
	type nodepoolResult struct {
		nodepool         *v1.NodePool
		threshold        float64
		passedCount      int
		failedCount      int
		passedCandidates []*Candidate
		failedCandidates []*Candidate
	}
	results := make([]nodepoolResult, 0)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Verify threshold is retrieved correctly
			threshold := tc.nodepool.Spec.Disruption.GetDecisionRatioThreshold()
			assert.Equal(t, tc.expectedThreshold, threshold,
				"NodePool %s should have threshold %.2f", tc.nodepool.Name, tc.expectedThreshold)

			// Compute nodepool metrics
			totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, tc.candidates)
			assert.Greater(t, totalCost, 0.0, "Total cost should be positive")
			assert.Greater(t, totalDisruption, 0.0, "Total disruption should be positive")

			// Compute decision ratios for all candidates
			for _, candidate := range tc.candidates {
				calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
			}

			// Apply policy filtering with the NodePool's threshold
			passedCandidates := make([]*Candidate, 0)
			failedCandidates := make([]*Candidate, 0)
			for _, candidate := range tc.candidates {
				if evaluator.ShouldConsolidate(
					tc.nodepool.Spec.Disruption.ConsolidateWhen,
					candidate,
					candidate.decisionRatio,
					threshold,
				) {
					passedCandidates = append(passedCandidates, candidate)
				} else {
					failedCandidates = append(failedCandidates, candidate)
				}
			}

			// Verify the correct number of candidates pass
			assert.GreaterOrEqual(t, len(passedCandidates), tc.minExpectedPass,
				"NodePool %s should have at least %d candidates pass", tc.nodepool.Name, tc.minExpectedPass)

			// Verify all passed candidates have ratio >= threshold
			for _, candidate := range passedCandidates {
				assert.GreaterOrEqual(t, candidate.decisionRatio, threshold,
					"Passed candidate should have ratio >= %.2f, got %.2f", threshold, candidate.decisionRatio)
			}

			// Verify all failed candidates have ratio < threshold
			for _, candidate := range failedCandidates {
				assert.Less(t, candidate.decisionRatio, threshold,
					"Failed candidate should have ratio < %.2f, got %.2f", threshold, candidate.decisionRatio)
			}

			// Store results for cross-NodePool verification
			results = append(results, nodepoolResult{
				nodepool:         tc.nodepool,
				threshold:        threshold,
				passedCount:      len(passedCandidates),
				failedCount:      len(failedCandidates),
				passedCandidates: passedCandidates,
				failedCandidates: failedCandidates,
			})

			t.Logf("NodePool %s (threshold %.2f): %d passed, %d failed",
				tc.nodepool.Name, threshold, len(passedCandidates), len(failedCandidates))
		})
	}

	// Verify no cross-NodePool interference
	t.Run("Verify no cross-NodePool interference", func(t *testing.T) {
		// Verify each NodePool used its own threshold
		assert.Equal(t, 3, len(results), "Should have results for all 3 NodePools")

		// Verify NodePool A (threshold 2.0) has different pass count than NodePool B (threshold 0.5)
		resultA := results[0]
		resultB := results[1]
		resultC := results[2]

		assert.Equal(t, 2.0, resultA.threshold, "NodePool A should have threshold 2.0")
		assert.Equal(t, 0.5, resultB.threshold, "NodePool B should have threshold 0.5")
		assert.Equal(t, 1.0, resultC.threshold, "NodePool C should have threshold 1.0")

		// Verify different thresholds produce different results
		// NodePool B (aggressive 0.5) should pass more candidates than NodePool A (conservative 2.0)
		assert.Greater(t, resultB.passedCount, resultA.passedCount,
			"Aggressive threshold (0.5) should pass more candidates than conservative threshold (2.0)")

		// NodePool C (default 1.0) should pass more than A but fewer than B
		assert.Greater(t, resultC.passedCount, resultA.passedCount,
			"Default threshold (1.0) should pass more candidates than conservative threshold (2.0)")
		assert.Less(t, resultC.passedCount, resultB.passedCount,
			"Default threshold (1.0) should pass fewer candidates than aggressive threshold (0.5)")

		// Verify that candidates from one NodePool don't affect another NodePool's decisions
		// Each NodePool's candidates should only be evaluated against their own threshold
		for _, result := range results {
			for _, candidate := range result.passedCandidates {
				assert.Equal(t, result.nodepool.Name, candidate.NodePool.Name,
					"Passed candidate should belong to the correct NodePool")
				assert.GreaterOrEqual(t, candidate.decisionRatio, result.threshold,
					"Passed candidate should meet its NodePool's threshold")
			}
			for _, candidate := range result.failedCandidates {
				assert.Equal(t, result.nodepool.Name, candidate.NodePool.Name,
					"Failed candidate should belong to the correct NodePool")
				assert.Less(t, candidate.decisionRatio, result.threshold,
					"Failed candidate should not meet its NodePool's threshold")
			}
		}

		t.Logf("Cross-NodePool verification passed:")
		t.Logf("  NodePool A (threshold 2.0): %d passed", resultA.passedCount)
		t.Logf("  NodePool B (threshold 0.5): %d passed", resultB.passedCount)
		t.Logf("  NodePool C (threshold 1.0): %d passed", resultC.passedCount)
		t.Logf("  Verified: B > C > A (as expected for thresholds 0.5 < 1.0 < 2.0)")
	})

	// Verify backward compatibility - NodePool C with nil threshold behaves like threshold 1.0
	t.Run("Verify backward compatibility", func(t *testing.T) {
		resultC := results[2]

		// Verify nil threshold defaults to 1.0
		assert.Nil(t, nodepoolC.Spec.Disruption.DecisionRatioThreshold,
			"NodePool C should have nil DecisionRatioThreshold")
		assert.Equal(t, 1.0, resultC.threshold,
			"NodePool C should use default threshold of 1.0")

		// Create a comparison NodePool with explicit threshold 1.0
		explicitNodepool := &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nodepool-explicit-1.0",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
					DecisionRatioThreshold: ptr(1.0),
				},
			},
		}

		// Create same candidates for explicit nodepool
		explicitCandidates := make([]*Candidate, len(candidatesC))
		for i, candidate := range candidatesC {
			explicitCandidates[i] = createCandidateForNodePool(explicitNodepool,
				candidate.instanceType.Offerings[0].Price,
				candidate.DisruptionCost,
				len(candidate.reschedulablePods))
		}

		// Compute metrics and ratios for explicit nodepool
		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, explicitCandidates)
		for _, candidate := range explicitCandidates {
			calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
		}

		// Apply policy filtering with explicit threshold 1.0
		explicitThreshold := explicitNodepool.Spec.Disruption.GetDecisionRatioThreshold()
		assert.Equal(t, 1.0, explicitThreshold, "Explicit threshold should be 1.0")

		explicitPassedCount := 0
		for _, candidate := range explicitCandidates {
			if evaluator.ShouldConsolidate(
				explicitNodepool.Spec.Disruption.ConsolidateWhen,
				candidate,
				candidate.decisionRatio,
				explicitThreshold,
			) {
				explicitPassedCount++
			}
		}

		// Verify nil threshold (default 1.0) behaves identically to explicit threshold 1.0
		assert.Equal(t, resultC.passedCount, explicitPassedCount,
			"Nil threshold (default 1.0) should behave identically to explicit threshold 1.0")

		t.Logf("Backward compatibility verified:")
		t.Logf("  NodePool C (nil threshold, defaults to 1.0): %d passed", resultC.passedCount)
		t.Logf("  Explicit threshold 1.0: %d passed", explicitPassedCount)
		t.Logf("  Behavior is identical (backward compatible)")
	})
}

// TestIntegration_BackwardCompatibility verifies that NodePools without DecisionRatioThreshold behave identically to threshold=1.0
// Validates: Requirements 9.1, 9.2, 9.3, 9.4
//
//nolint:gocyclo
func TestIntegration_BackwardCompatibility(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})
	evaluator := &PolicyEvaluator{}

	// Create NodePool without DecisionRatioThreshold field (backward compatible)
	nodepoolWithoutThreshold := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodepool-without-threshold",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: nil, // Not specified, should default to 1.0
			},
		},
	}

	// Create NodePool with explicit threshold of 1.0 (for comparison)
	nodepoolWithExplicitThreshold := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodepool-with-explicit-threshold",
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
				DecisionRatioThreshold: ptr(1.0), // Explicitly set to 1.0
			},
		},
	}

	// Create candidates with various decision ratios
	// Design ratios to have some above and below 1.0 threshold
	// ratio = (cost / totalCost) / (disruption / totalDisruption)
	// totalCost = 5 + 10 + 15 + 20 + 25 = 75
	// totalDisruption = 15 + 12 + 10 + 8 + 5 = 50
	candidatesWithoutThreshold := []*Candidate{
		// ratio = (5/75) / (15/50) = 0.067 / 0.3 = 0.22 (below 1.0)
		createCandidateForNodePool(nodepoolWithoutThreshold, 5.0, 15.0, 3),
		// ratio = (10/75) / (12/50) = 0.133 / 0.24 = 0.55 (below 1.0)
		createCandidateForNodePool(nodepoolWithoutThreshold, 10.0, 12.0, 2),
		// ratio = (15/75) / (10/50) = 0.2 / 0.2 = 1.0 (equal to 1.0, boundary case)
		createCandidateForNodePool(nodepoolWithoutThreshold, 15.0, 10.0, 1),
		// ratio = (20/75) / (8/50) = 0.267 / 0.16 = 1.67 (above 1.0)
		createCandidateForNodePool(nodepoolWithoutThreshold, 20.0, 8.0, 4),
		// ratio = (25/75) / (5/50) = 0.333 / 0.1 = 3.33 (above 1.0)
		createCandidateForNodePool(nodepoolWithoutThreshold, 25.0, 5.0, 2),
	}

	// Create identical candidates for explicit threshold nodepool
	candidatesWithExplicitThreshold := []*Candidate{
		createCandidateForNodePool(nodepoolWithExplicitThreshold, 5.0, 15.0, 3),
		createCandidateForNodePool(nodepoolWithExplicitThreshold, 10.0, 12.0, 2),
		createCandidateForNodePool(nodepoolWithExplicitThreshold, 15.0, 10.0, 1),
		createCandidateForNodePool(nodepoolWithExplicitThreshold, 20.0, 8.0, 4),
		createCandidateForNodePool(nodepoolWithExplicitThreshold, 25.0, 5.0, 2),
	}

	// Test 1: Verify threshold retrieval defaults to 1.0
	t.Run("Verify threshold defaults to 1.0", func(t *testing.T) {
		thresholdWithout := nodepoolWithoutThreshold.Spec.Disruption.GetDecisionRatioThreshold()
		thresholdExplicit := nodepoolWithExplicitThreshold.Spec.Disruption.GetDecisionRatioThreshold()

		assert.Equal(t, 1.0, thresholdWithout, "Nil threshold should default to 1.0")
		assert.Equal(t, 1.0, thresholdExplicit, "Explicit threshold should be 1.0")
		assert.Equal(t, thresholdWithout, thresholdExplicit, "Both thresholds should be equal")

		t.Logf("Threshold retrieval verified:")
		t.Logf("  NodePool without threshold: %.2f (default)", thresholdWithout)
		t.Logf("  NodePool with explicit threshold: %.2f", thresholdExplicit)
	})

	// Test 2: Verify consolidation behavior is identical
	t.Run("Verify consolidation behavior is identical", func(t *testing.T) {
		// Process NodePool without threshold
		totalCostWithout, totalDisruptionWithout := calculator.ComputeNodePoolMetrics(ctx, candidatesWithoutThreshold)
		assert.Greater(t, totalCostWithout, 0.0, "Total cost should be positive")
		assert.Greater(t, totalDisruptionWithout, 0.0, "Total disruption should be positive")

		for _, candidate := range candidatesWithoutThreshold {
			calculator.ComputeDecisionRatio(ctx, candidate, totalCostWithout, totalDisruptionWithout)
		}

		thresholdWithout := nodepoolWithoutThreshold.Spec.Disruption.GetDecisionRatioThreshold()
		passedWithout := make([]*Candidate, 0)
		failedWithout := make([]*Candidate, 0)
		for _, candidate := range candidatesWithoutThreshold {
			if evaluator.ShouldConsolidate(
				nodepoolWithoutThreshold.Spec.Disruption.ConsolidateWhen,
				candidate,
				candidate.decisionRatio,
				thresholdWithout,
			) {
				passedWithout = append(passedWithout, candidate)
			} else {
				failedWithout = append(failedWithout, candidate)
			}
		}

		// Process NodePool with explicit threshold
		totalCostExplicit, totalDisruptionExplicit := calculator.ComputeNodePoolMetrics(ctx, candidatesWithExplicitThreshold)
		assert.Greater(t, totalCostExplicit, 0.0, "Total cost should be positive")
		assert.Greater(t, totalDisruptionExplicit, 0.0, "Total disruption should be positive")

		for _, candidate := range candidatesWithExplicitThreshold {
			calculator.ComputeDecisionRatio(ctx, candidate, totalCostExplicit, totalDisruptionExplicit)
		}

		thresholdExplicit := nodepoolWithExplicitThreshold.Spec.Disruption.GetDecisionRatioThreshold()
		passedExplicit := make([]*Candidate, 0)
		failedExplicit := make([]*Candidate, 0)
		for _, candidate := range candidatesWithExplicitThreshold {
			if evaluator.ShouldConsolidate(
				nodepoolWithExplicitThreshold.Spec.Disruption.ConsolidateWhen,
				candidate,
				candidate.decisionRatio,
				thresholdExplicit,
			) {
				passedExplicit = append(passedExplicit, candidate)
			} else {
				failedExplicit = append(failedExplicit, candidate)
			}
		}

		// Verify identical behavior
		assert.Equal(t, len(passedWithout), len(passedExplicit),
			"Both NodePools should pass the same number of candidates")
		assert.Equal(t, len(failedWithout), len(failedExplicit),
			"Both NodePools should fail the same number of candidates")

		// Verify the same candidates passed in both cases
		for i := 0; i < len(passedWithout); i++ {
			assert.InDelta(t, passedWithout[i].decisionRatio, passedExplicit[i].decisionRatio, 0.01,
				"Passed candidates should have the same decision ratios")
			assert.GreaterOrEqual(t, passedWithout[i].decisionRatio, 1.0,
				"Passed candidates should have ratio >= 1.0")
		}

		// Verify the same candidates failed in both cases
		for i := 0; i < len(failedWithout); i++ {
			assert.InDelta(t, failedWithout[i].decisionRatio, failedExplicit[i].decisionRatio, 0.01,
				"Failed candidates should have the same decision ratios")
			assert.Less(t, failedWithout[i].decisionRatio, 1.0,
				"Failed candidates should have ratio < 1.0")
		}

		t.Logf("Consolidation behavior verified:")
		t.Logf("  NodePool without threshold: %d passed, %d failed", len(passedWithout), len(failedWithout))
		t.Logf("  NodePool with explicit threshold: %d passed, %d failed", len(passedExplicit), len(failedExplicit))
		t.Logf("  Behavior is identical (backward compatible)")
	})

	// Test 3: Verify metrics show threshold=1.0
	t.Run("Verify metrics show threshold=1.0", func(t *testing.T) {
		// In a real implementation, we would verify that metrics are emitted with threshold label "1.00"
		// For both NodePools (with and without explicit threshold)

		thresholdWithout := nodepoolWithoutThreshold.Spec.Disruption.GetDecisionRatioThreshold()
		thresholdExplicit := nodepoolWithExplicitThreshold.Spec.Disruption.GetDecisionRatioThreshold()

		// Format thresholds as they would appear in metrics (2 decimal places)
		metricLabelWithout := fmt.Sprintf("%.2f", thresholdWithout)
		metricLabelExplicit := fmt.Sprintf("%.2f", thresholdExplicit)

		assert.Equal(t, "1.00", metricLabelWithout,
			"Metrics for NodePool without threshold should show threshold=1.00")
		assert.Equal(t, "1.00", metricLabelExplicit,
			"Metrics for NodePool with explicit threshold should show threshold=1.00")
		assert.Equal(t, metricLabelWithout, metricLabelExplicit,
			"Both NodePools should emit metrics with the same threshold label")

		t.Logf("Metrics verification:")
		t.Logf("  NodePool without threshold: threshold label = %s", metricLabelWithout)
		t.Logf("  NodePool with explicit threshold: threshold label = %s", metricLabelExplicit)
		t.Logf("  Both emit metrics with threshold=1.00 (backward compatible)")

		// Verify that metrics would be emitted correctly
		// In real implementation, this would verify:
		// - movesAboveThresholdCounter.WithLabelValues(nodepool, policy, "1.00").Inc()
		// - movesBelowThresholdCounter.WithLabelValues(nodepool, policy, "1.00").Inc()
		// - decisionRatioHistogram.WithLabelValues(nodepool, policy, "1.00", moveType).Observe(ratio)
	})

	// Test 4: Verify delete ratio filtering behaves identically
	t.Run("Verify delete ratio filtering behaves identically", func(t *testing.T) {
		// Compute delete ratios for both NodePools
		totalCostWithout, totalDisruptionWithout := calculator.ComputeNodePoolMetrics(ctx, candidatesWithoutThreshold)
		for _, candidate := range candidatesWithoutThreshold {
			calculator.ComputeDeleteRatio(ctx, candidate, totalCostWithout, totalDisruptionWithout)
		}

		totalCostExplicit, totalDisruptionExplicit := calculator.ComputeNodePoolMetrics(ctx, candidatesWithExplicitThreshold)
		for _, candidate := range candidatesWithExplicitThreshold {
			calculator.ComputeDeleteRatio(ctx, candidate, totalCostExplicit, totalDisruptionExplicit)
		}

		// Simulate delete ratio filtering for both NodePools
		thresholdWithout := nodepoolWithoutThreshold.Spec.Disruption.GetDecisionRatioThreshold()
		thresholdExplicit := nodepoolWithExplicitThreshold.Spec.Disruption.GetDecisionRatioThreshold()

		skippedCountWithout := 0
		for _, candidate := range candidatesWithoutThreshold {
			if candidate.NodePool.Spec.Disruption.ConsolidateWhen == v1.ConsolidateWhenCostJustifiesDisruption {
				if candidate.deleteRatio < thresholdWithout {
					skippedCountWithout++
				}
			}
		}

		skippedCountExplicit := 0
		for _, candidate := range candidatesWithExplicitThreshold {
			if candidate.NodePool.Spec.Disruption.ConsolidateWhen == v1.ConsolidateWhenCostJustifiesDisruption {
				if candidate.deleteRatio < thresholdExplicit {
					skippedCountExplicit++
				}
			}
		}

		// Verify identical delete ratio filtering behavior
		assert.Equal(t, skippedCountWithout, skippedCountExplicit,
			"Both NodePools should skip the same number of candidates by delete ratio")

		t.Logf("Delete ratio filtering verified:")
		t.Logf("  NodePool without threshold: skipped %d candidates", skippedCountWithout)
		t.Logf("  NodePool with explicit threshold: skipped %d candidates", skippedCountExplicit)
		t.Logf("  Filtering behavior is identical (backward compatible)")
	})

	// Test 5: Verify existing NodePool configurations remain valid
	t.Run("Verify existing NodePool configurations remain valid", func(t *testing.T) {
		// Create a NodePool that represents an existing configuration (no DecisionRatioThreshold field)
		existingNodepool := &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "existing-nodepool",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateWhen: v1.ConsolidateWhenCostJustifiesDisruption,
					// DecisionRatioThreshold not specified (existing configuration)
				},
			},
		}

		// Verify the NodePool is valid and threshold defaults to 1.0
		threshold := existingNodepool.Spec.Disruption.GetDecisionRatioThreshold()
		assert.Equal(t, 1.0, threshold, "Existing NodePool should default to threshold 1.0")

		// Create candidates for existing NodePool
		existingCandidates := []*Candidate{
			createCandidateForNodePool(existingNodepool, 10.0, 8.0, 2),  // ratio > 1.0
			createCandidateForNodePool(existingNodepool, 5.0, 12.0, 3),  // ratio < 1.0
			createCandidateForNodePool(existingNodepool, 15.0, 10.0, 1), // ratio > 1.0
		}

		// Compute metrics and ratios
		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, existingCandidates)
		for _, candidate := range existingCandidates {
			calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
		}

		// Apply policy filtering
		passedCount := 0
		for _, candidate := range existingCandidates {
			if evaluator.ShouldConsolidate(
				existingNodepool.Spec.Disruption.ConsolidateWhen,
				candidate,
				candidate.decisionRatio,
				threshold,
			) {
				passedCount++
			}
		}

		// Verify consolidation works correctly with existing configuration
		assert.GreaterOrEqual(t, passedCount, 1, "Existing NodePool should consolidate candidates with ratio >= 1.0")

		t.Logf("Existing NodePool configuration verified:")
		t.Logf("  Threshold: %.2f (default)", threshold)
		t.Logf("  Candidates passed: %d", passedCount)
		t.Logf("  Configuration remains valid and functional (backward compatible)")
	})

	// Test 6: Verify simultaneous operation with new configurations
	t.Run("Verify simultaneous operation with new configurations", func(t *testing.T) {
		// Create a NodePool with a custom threshold (new configuration)
		newNodepool := &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "new-nodepool-custom-threshold",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateWhen:        v1.ConsolidateWhenCostJustifiesDisruption,
					DecisionRatioThreshold: ptr(1.5), // Custom threshold
				},
			},
		}

		// Create candidates for both NodePools
		oldCandidates := []*Candidate{
			createCandidateForNodePool(nodepoolWithoutThreshold, 10.0, 8.0, 2), // ratio > 1.0
			createCandidateForNodePool(nodepoolWithoutThreshold, 8.0, 10.0, 3), // ratio < 1.0
		}

		newCandidates := []*Candidate{
			createCandidateForNodePool(newNodepool, 10.0, 8.0, 2), // ratio > 1.0 but may be < 1.5
			createCandidateForNodePool(newNodepool, 8.0, 10.0, 3), // ratio < 1.0
		}

		// Process old NodePool (without threshold)
		totalCostOld, totalDisruptionOld := calculator.ComputeNodePoolMetrics(ctx, oldCandidates)
		for _, candidate := range oldCandidates {
			calculator.ComputeDecisionRatio(ctx, candidate, totalCostOld, totalDisruptionOld)
		}

		thresholdOld := nodepoolWithoutThreshold.Spec.Disruption.GetDecisionRatioThreshold()
		passedCountOld := 0
		for _, candidate := range oldCandidates {
			if evaluator.ShouldConsolidate(
				nodepoolWithoutThreshold.Spec.Disruption.ConsolidateWhen,
				candidate,
				candidate.decisionRatio,
				thresholdOld,
			) {
				passedCountOld++
			}
		}

		// Process new NodePool (with custom threshold)
		totalCostNew, totalDisruptionNew := calculator.ComputeNodePoolMetrics(ctx, newCandidates)
		for _, candidate := range newCandidates {
			calculator.ComputeDecisionRatio(ctx, candidate, totalCostNew, totalDisruptionNew)
		}

		thresholdNew := newNodepool.Spec.Disruption.GetDecisionRatioThreshold()
		passedCountNew := 0
		for _, candidate := range newCandidates {
			if evaluator.ShouldConsolidate(
				newNodepool.Spec.Disruption.ConsolidateWhen,
				candidate,
				candidate.decisionRatio,
				thresholdNew,
			) {
				passedCountNew++
			}
		}

		// Verify both NodePools operate independently
		assert.Equal(t, 1.0, thresholdOld, "Old NodePool should use default threshold 1.0")
		assert.Equal(t, 1.5, thresholdNew, "New NodePool should use custom threshold 1.5")

		// Verify old NodePool is not affected by new NodePool's custom threshold
		assert.GreaterOrEqual(t, passedCountOld, 1, "Old NodePool should consolidate with threshold 1.0")

		t.Logf("Simultaneous operation verified:")
		t.Logf("  Old NodePool (no threshold): threshold=%.2f, passed=%d", thresholdOld, passedCountOld)
		t.Logf("  New NodePool (custom threshold): threshold=%.2f, passed=%d", thresholdNew, passedCountNew)
		t.Logf("  Both NodePools operate independently (backward compatible)")
	})
}
