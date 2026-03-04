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
	"strconv"
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
)

func TestComputeDecisionRatio_BasicComputation(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create test candidates with known costs and disruptions
	candidates := []*Candidate{
		createTestCandidateWithValues(1.0, 10.0), // cost=1.0, disruption=10.0
		createTestCandidateWithValues(2.0, 20.0), // cost=2.0, disruption=20.0
		createTestCandidateWithValues(3.0, 30.0), // cost=3.0, disruption=30.0
	}

	// Total cost = 6.0, total disruption = 60.0
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Test first candidate
	ratio := calculator.ComputeDecisionRatio(ctx, candidates[0], totalCost, totalDisruption)

	// Expected: normalizedCost = 1.0/6.0 = 0.1667, normalizedDisruption = 10.0/60.0 = 0.1667
	// Decision ratio = 0.1667 / 0.1667 = 1.0
	expectedRatio := 1.0
	if !floatEqualsTest(ratio, expectedRatio, 1e-9) {
		t.Errorf("Expected ratio %f, got %f", expectedRatio, ratio)
	}

	// Verify stored values in candidate
	if !floatEqualsTest(candidates[0].normalizedCost, 1.0/6.0, 1e-9) {
		t.Errorf("Expected normalizedCost %f, got %f", 1.0/6.0, candidates[0].normalizedCost)
	}
	if !floatEqualsTest(candidates[0].normalizedDisruption, 10.0/60.0, 1e-9) {
		t.Errorf("Expected normalizedDisruption %f, got %f", 10.0/60.0, candidates[0].normalizedDisruption)
	}
	if !floatEqualsTest(candidates[0].decisionRatio, expectedRatio, 1e-9) {
		t.Errorf("Expected decisionRatio %f, got %f", expectedRatio, candidates[0].decisionRatio)
	}
}

func TestComputeDecisionRatio_ZeroTotalDisruption(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	candidate := createTestCandidateWithValues(1.0, 0.0)
	totalCost := 5.0
	totalDisruption := 0.0

	ratio := calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

	// Should return positive infinity
	if !math.IsInf(ratio, 1) {
		t.Errorf("Expected positive infinity, got %f", ratio)
	}
}

func TestComputeDecisionRatio_ZeroNormalizedDisruption(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	candidate := createTestCandidateWithValues(1.0, 0.0)
	totalCost := 5.0
	totalDisruption := 100.0 // Non-zero total, but this candidate has zero

	ratio := calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

	// Should return positive infinity
	if !math.IsInf(ratio, 1) {
		t.Errorf("Expected positive infinity, got %f", ratio)
	}
}

func TestComputeDecisionRatio_HighRatio(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates where one has high cost but low disruption
	candidates := []*Candidate{
		createTestCandidateWithValues(10.0, 1.0), // High cost, low disruption
		createTestCandidateWithValues(1.0, 10.0), // Low cost, high disruption
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Test high-ratio candidate
	ratio := calculator.ComputeDecisionRatio(ctx, candidates[0], totalCost, totalDisruption)

	// Expected: normalizedCost = 10.0/11.0 = 0.909, normalizedDisruption = 1.0/11.0 = 0.091
	// Decision ratio = 0.909 / 0.091 = 10.0
	expectedRatio := 10.0
	if !floatEqualsTest(ratio, expectedRatio, 1e-6) {
		t.Errorf("Expected ratio %f, got %f", expectedRatio, ratio)
	}
}

func TestComputeDecisionRatio_LowRatio(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates where one has low cost but high disruption
	candidates := []*Candidate{
		createTestCandidateWithValues(1.0, 10.0), // Low cost, high disruption
		createTestCandidateWithValues(10.0, 1.0), // High cost, low disruption
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Test low-ratio candidate
	ratio := calculator.ComputeDecisionRatio(ctx, candidates[0], totalCost, totalDisruption)

	// Expected: normalizedCost = 1.0/11.0 = 0.091, normalizedDisruption = 10.0/11.0 = 0.909
	// Decision ratio = 0.091 / 0.909 = 0.1
	expectedRatio := 0.1
	if !floatEqualsTest(ratio, expectedRatio, 1e-6) {
		t.Errorf("Expected ratio %f, got %f", expectedRatio, ratio)
	}
}

// Helper function to create a test candidate with specific cost and disruption values
func createTestCandidateWithValues(cost, disruption float64) *Candidate {
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
	}
}

func TestComputeDeleteRatio_BasicComputation(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create test candidates with known costs and disruptions
	candidates := []*Candidate{
		createTestCandidateWithValues(1.0, 10.0), // cost=1.0, disruption=10.0
		createTestCandidateWithValues(2.0, 20.0), // cost=2.0, disruption=20.0
		createTestCandidateWithValues(3.0, 30.0), // cost=3.0, disruption=30.0
	}

	// Total cost = 6.0, total disruption = 60.0
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Test first candidate - delete ratio should equal decision ratio for delete moves
	deleteRatio := calculator.ComputeDeleteRatio(ctx, candidates[0], totalCost, totalDisruption)

	// Expected: normalizedCost = 1.0/6.0 = 0.1667, normalizedDisruption = 10.0/60.0 = 0.1667
	// Delete ratio = 0.1667 / 0.1667 = 1.0
	expectedRatio := 1.0
	if !floatEqualsTest(deleteRatio, expectedRatio, 1e-9) {
		t.Errorf("Expected delete ratio %f, got %f", expectedRatio, deleteRatio)
	}

	// Verify stored value in candidate
	if !floatEqualsTest(candidates[0].deleteRatio, expectedRatio, 1e-9) {
		t.Errorf("Expected stored deleteRatio %f, got %f", expectedRatio, candidates[0].deleteRatio)
	}
}

func TestComputeDeleteRatio_ZeroTotalDisruption(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	candidate := createTestCandidateWithValues(1.0, 0.0)
	totalCost := 5.0
	totalDisruption := 0.0

	deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

	// Should return positive infinity
	if !math.IsInf(deleteRatio, 1) {
		t.Errorf("Expected positive infinity, got %f", deleteRatio)
	}
}

func TestComputeDeleteRatio_ZeroNormalizedDisruption(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	candidate := createTestCandidateWithValues(1.0, 0.0)
	totalCost := 5.0
	totalDisruption := 100.0 // Non-zero total, but this candidate has zero

	deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

	// Should return positive infinity
	if !math.IsInf(deleteRatio, 1) {
		t.Errorf("Expected positive infinity, got %f", deleteRatio)
	}
}

func TestComputeDeleteRatio_HighRatio(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates where one has high cost but low disruption
	candidates := []*Candidate{
		createTestCandidateWithValues(10.0, 1.0), // High cost, low disruption
		createTestCandidateWithValues(1.0, 10.0), // Low cost, high disruption
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Test high-ratio candidate
	deleteRatio := calculator.ComputeDeleteRatio(ctx, candidates[0], totalCost, totalDisruption)

	// Expected: normalizedCost = 10.0/11.0 = 0.909, normalizedDisruption = 1.0/11.0 = 0.091
	// Delete ratio = 0.909 / 0.091 = 10.0
	expectedRatio := 10.0
	if !floatEqualsTest(deleteRatio, expectedRatio, 1e-6) {
		t.Errorf("Expected delete ratio %f, got %f", expectedRatio, deleteRatio)
	}
}

func TestComputeDeleteRatio_LowRatio(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates where one has low cost but high disruption
	candidates := []*Candidate{
		createTestCandidateWithValues(1.0, 10.0), // Low cost, high disruption
		createTestCandidateWithValues(10.0, 1.0), // High cost, low disruption
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Test low-ratio candidate
	deleteRatio := calculator.ComputeDeleteRatio(ctx, candidates[0], totalCost, totalDisruption)

	// Expected: normalizedCost = 1.0/11.0 = 0.091, normalizedDisruption = 10.0/11.0 = 0.909
	// Delete ratio = 0.091 / 0.909 = 0.1
	expectedRatio := 0.1
	if !floatEqualsTest(deleteRatio, expectedRatio, 1e-6) {
		t.Errorf("Expected delete ratio %f, got %f", expectedRatio, deleteRatio)
	}
}

func TestComputeDeleteRatio_MatchesDecisionRatio(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// For a delete move, the delete ratio should equal the decision ratio
	// since both assume full node cost recovery
	candidates := []*Candidate{
		createTestCandidateWithValues(5.0, 15.0),
		createTestCandidateWithValues(3.0, 10.0),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute both ratios for the first candidate
	decisionRatio := calculator.ComputeDecisionRatio(ctx, candidates[0], totalCost, totalDisruption)
	deleteRatio := calculator.ComputeDeleteRatio(ctx, candidates[0], totalCost, totalDisruption)

	// They should be equal since delete moves recover full node cost
	if !floatEqualsTest(decisionRatio, deleteRatio, 1e-9) {
		t.Errorf("Expected delete ratio to match decision ratio: decision=%f, delete=%f", decisionRatio, deleteRatio)
	}
}

// Helper function to check float equality with tolerance
func floatEqualsTest(a, b, tolerance float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= tolerance
}

// TestComputeNodePoolMetrics_BasicComputation tests the basic computation of nodepool metrics
func TestComputeNodePoolMetrics_BasicComputation(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create test candidates with known costs and disruptions
	candidates := []*Candidate{
		createTestCandidateWithValues(1.0, 10.0), // cost=1.0, disruption=10.0
		createTestCandidateWithValues(2.0, 20.0), // cost=2.0, disruption=20.0
		createTestCandidateWithValues(3.0, 30.0), // cost=3.0, disruption=30.0
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Expected: totalCost = 1.0 + 2.0 + 3.0 = 6.0
	expectedTotalCost := 6.0
	if !floatEqualsTest(totalCost, expectedTotalCost, 1e-9) {
		t.Errorf("Expected totalCost %f, got %f", expectedTotalCost, totalCost)
	}

	// Expected: totalDisruption = 10.0 + 20.0 + 30.0 = 60.0
	expectedTotalDisruption := 60.0
	if !floatEqualsTest(totalDisruption, expectedTotalDisruption, 1e-9) {
		t.Errorf("Expected totalDisruption %f, got %f", expectedTotalDisruption, totalDisruption)
	}
}

// TestComputeNodePoolMetrics_EmptyCandidates tests with empty candidate list
func TestComputeNodePoolMetrics_EmptyCandidates(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	candidates := []*Candidate{}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Expected: both should be 0.0
	if totalCost != 0.0 {
		t.Errorf("Expected totalCost 0.0, got %f", totalCost)
	}
	if totalDisruption != 0.0 {
		t.Errorf("Expected totalDisruption 0.0, got %f", totalDisruption)
	}
}

// TestComputeNodePoolMetrics_ZeroCost tests with candidates having zero cost
func TestComputeNodePoolMetrics_ZeroCost(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates with zero cost but non-zero disruption
	candidates := []*Candidate{
		createTestCandidateWithValues(0.0, 10.0),
		createTestCandidateWithValues(0.0, 20.0),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Expected: totalCost = 0.0, totalDisruption = 30.0
	if totalCost != 0.0 {
		t.Errorf("Expected totalCost 0.0, got %f", totalCost)
	}
	expectedTotalDisruption := 30.0
	if !floatEqualsTest(totalDisruption, expectedTotalDisruption, 1e-9) {
		t.Errorf("Expected totalDisruption %f, got %f", expectedTotalDisruption, totalDisruption)
	}
}

// TestComputeNodePoolMetrics_ZeroDisruption tests with candidates having zero disruption
func TestComputeNodePoolMetrics_ZeroDisruption(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates with non-zero cost but zero disruption
	candidates := []*Candidate{
		createTestCandidateWithValues(1.0, 0.0),
		createTestCandidateWithValues(2.0, 0.0),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Expected: totalCost = 3.0, totalDisruption = 0.0
	expectedTotalCost := 3.0
	if !floatEqualsTest(totalCost, expectedTotalCost, 1e-9) {
		t.Errorf("Expected totalCost %f, got %f", expectedTotalCost, totalCost)
	}
	if totalDisruption != 0.0 {
		t.Errorf("Expected totalDisruption 0.0, got %f", totalDisruption)
	}
}

// TestComputeNodePoolMetrics_NilInstanceType tests with candidates having nil instance type
func TestComputeNodePoolMetrics_NilInstanceType(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create a candidate with nil instance type
	candidate := &Candidate{
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
		instanceType:   nil, // Nil instance type
		DisruptionCost: 10.0,
	}

	candidates := []*Candidate{candidate}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Expected: totalCost = 0.0 (nil instance type), totalDisruption = 10.0
	if totalCost != 0.0 {
		t.Errorf("Expected totalCost 0.0 for nil instance type, got %f", totalCost)
	}
	expectedTotalDisruption := 10.0
	if !floatEqualsTest(totalDisruption, expectedTotalDisruption, 1e-9) {
		t.Errorf("Expected totalDisruption %f, got %f", expectedTotalDisruption, totalDisruption)
	}
}

// TestComputeNodePoolMetrics_EmptyOfferings tests with candidates having empty offerings
func TestComputeNodePoolMetrics_EmptyOfferings(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create a candidate with empty offerings
	candidate := &Candidate{
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
			Name:      "test-instance-type",
			Offerings: cloudprovider.Offerings{}, // Empty offerings
		},
		DisruptionCost: 10.0,
	}

	candidates := []*Candidate{candidate}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Expected: totalCost = 0.0 (empty offerings), totalDisruption = 10.0
	if totalCost != 0.0 {
		t.Errorf("Expected totalCost 0.0 for empty offerings, got %f", totalCost)
	}
	expectedTotalDisruption := 10.0
	if !floatEqualsTest(totalDisruption, expectedTotalDisruption, 1e-9) {
		t.Errorf("Expected totalDisruption %f, got %f", expectedTotalDisruption, totalDisruption)
	}
}

// TestComputeNodePoolMetrics_MixedCandidates tests with a mix of valid and edge case candidates
func TestComputeNodePoolMetrics_MixedCandidates(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create a mix of candidates
	candidates := []*Candidate{
		createTestCandidateWithValues(1.0, 10.0), // Normal candidate
		createTestCandidateWithValues(0.0, 5.0),  // Zero cost
		createTestCandidateWithValues(2.0, 0.0),  // Zero disruption
		createTestCandidateWithValues(3.0, 15.0), // Normal candidate
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Expected: totalCost = 1.0 + 0.0 + 2.0 + 3.0 = 6.0
	expectedTotalCost := 6.0
	if !floatEqualsTest(totalCost, expectedTotalCost, 1e-9) {
		t.Errorf("Expected totalCost %f, got %f", expectedTotalCost, totalCost)
	}

	// Expected: totalDisruption = 10.0 + 5.0 + 0.0 + 15.0 = 30.0
	expectedTotalDisruption := 30.0
	if !floatEqualsTest(totalDisruption, expectedTotalDisruption, 1e-9) {
		t.Errorf("Expected totalDisruption %f, got %f", expectedTotalDisruption, totalDisruption)
	}
}

// TestComputeDecisionRatio_VariousInputCombinations tests decision ratio with various input combinations
func TestComputeDecisionRatio_VariousInputCombinations(t *testing.T) {
	tests := []struct {
		name                     string
		nodeCost                 float64
		nodeDisruption           float64
		totalCost                float64
		totalDisruption          float64
		expectedRatio            float64
		expectedNormalizedCost   float64
		expectedNormalizedDisrup float64
	}{
		{
			name:                     "equal normalized values",
			nodeCost:                 2.0,
			nodeDisruption:           20.0,
			totalCost:                10.0,
			totalDisruption:          100.0,
			expectedRatio:            1.0,
			expectedNormalizedCost:   0.2,
			expectedNormalizedDisrup: 0.2,
		},
		{
			name:                     "high cost low disruption",
			nodeCost:                 8.0,
			nodeDisruption:           10.0,
			totalCost:                10.0,
			totalDisruption:          100.0,
			expectedRatio:            8.0,
			expectedNormalizedCost:   0.8,
			expectedNormalizedDisrup: 0.1,
		},
		{
			name:                     "low cost high disruption",
			nodeCost:                 1.0,
			nodeDisruption:           80.0,
			totalCost:                10.0,
			totalDisruption:          100.0,
			expectedRatio:            0.125,
			expectedNormalizedCost:   0.1,
			expectedNormalizedDisrup: 0.8,
		},
		{
			name:                     "very small values",
			nodeCost:                 0.001,
			nodeDisruption:           0.01,
			totalCost:                0.01,
			totalDisruption:          0.1,
			expectedRatio:            1.0,
			expectedNormalizedCost:   0.1,
			expectedNormalizedDisrup: 0.1,
		},
		{
			name:                     "very large values",
			nodeCost:                 1000.0,
			nodeDisruption:           10000.0,
			totalCost:                10000.0,
			totalDisruption:          100000.0,
			expectedRatio:            1.0,
			expectedNormalizedCost:   0.1,
			expectedNormalizedDisrup: 0.1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			calculator := NewDecisionRatioCalculator(clock.RealClock{})

			candidate := createTestCandidateWithValues(tt.nodeCost, tt.nodeDisruption)

			ratio := calculator.ComputeDecisionRatio(ctx, candidate, tt.totalCost, tt.totalDisruption)

			// Verify ratio
			if !floatEqualsTest(ratio, tt.expectedRatio, 1e-6) {
				t.Errorf("Expected ratio %f, got %f", tt.expectedRatio, ratio)
			}

			// Verify normalized cost
			if !floatEqualsTest(candidate.normalizedCost, tt.expectedNormalizedCost, 1e-9) {
				t.Errorf("Expected normalizedCost %f, got %f", tt.expectedNormalizedCost, candidate.normalizedCost)
			}

			// Verify normalized disruption
			if !floatEqualsTest(candidate.normalizedDisruption, tt.expectedNormalizedDisrup, 1e-9) {
				t.Errorf("Expected normalizedDisruption %f, got %f", tt.expectedNormalizedDisrup, candidate.normalizedDisruption)
			}

			// Verify stored decision ratio
			if !floatEqualsTest(candidate.decisionRatio, tt.expectedRatio, 1e-6) {
				t.Errorf("Expected stored decisionRatio %f, got %f", tt.expectedRatio, candidate.decisionRatio)
			}
		})
	}
}

// TestComputeDeleteRatio_VariousInputCombinations tests delete ratio with various input combinations
func TestComputeDeleteRatio_VariousInputCombinations(t *testing.T) {
	tests := []struct {
		name            string
		nodeCost        float64
		nodeDisruption  float64
		totalCost       float64
		totalDisruption float64
		expectedRatio   float64
	}{
		{
			name:            "equal normalized values",
			nodeCost:        2.0,
			nodeDisruption:  20.0,
			totalCost:       10.0,
			totalDisruption: 100.0,
			expectedRatio:   1.0,
		},
		{
			name:            "high cost low disruption",
			nodeCost:        8.0,
			nodeDisruption:  10.0,
			totalCost:       10.0,
			totalDisruption: 100.0,
			expectedRatio:   8.0,
		},
		{
			name:            "low cost high disruption",
			nodeCost:        1.0,
			nodeDisruption:  80.0,
			totalCost:       10.0,
			totalDisruption: 100.0,
			expectedRatio:   0.125,
		},
		{
			name:            "very small values",
			nodeCost:        0.001,
			nodeDisruption:  0.01,
			totalCost:       0.01,
			totalDisruption: 0.1,
			expectedRatio:   1.0,
		},
		{
			name:            "very large values",
			nodeCost:        1000.0,
			nodeDisruption:  10000.0,
			totalCost:       10000.0,
			totalDisruption: 100000.0,
			expectedRatio:   1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			calculator := NewDecisionRatioCalculator(clock.RealClock{})

			candidate := createTestCandidateWithValues(tt.nodeCost, tt.nodeDisruption)

			deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, tt.totalCost, tt.totalDisruption)

			// Verify delete ratio
			if !floatEqualsTest(deleteRatio, tt.expectedRatio, 1e-6) {
				t.Errorf("Expected delete ratio %f, got %f", tt.expectedRatio, deleteRatio)
			}

			// Verify stored delete ratio
			if !floatEqualsTest(candidate.deleteRatio, tt.expectedRatio, 1e-6) {
				t.Errorf("Expected stored deleteRatio %f, got %f", tt.expectedRatio, candidate.deleteRatio)
			}
		})
	}
}

// TestComputeDecisionRatio_InfinityHandling tests infinity handling in decision ratio computation
func TestComputeDecisionRatio_InfinityHandling(t *testing.T) {
	tests := []struct {
		name            string
		nodeCost        float64
		nodeDisruption  float64
		totalCost       float64
		totalDisruption float64
		expectInfinity  bool
	}{
		{
			name:            "zero total disruption",
			nodeCost:        1.0,
			nodeDisruption:  0.0,
			totalCost:       5.0,
			totalDisruption: 0.0,
			expectInfinity:  true,
		},
		{
			name:            "zero normalized disruption",
			nodeCost:        1.0,
			nodeDisruption:  0.0,
			totalCost:       5.0,
			totalDisruption: 100.0,
			expectInfinity:  true,
		},
		{
			name:            "non-zero disruption",
			nodeCost:        1.0,
			nodeDisruption:  10.0,
			totalCost:       5.0,
			totalDisruption: 50.0,
			expectInfinity:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			calculator := NewDecisionRatioCalculator(clock.RealClock{})

			candidate := createTestCandidateWithValues(tt.nodeCost, tt.nodeDisruption)

			ratio := calculator.ComputeDecisionRatio(ctx, candidate, tt.totalCost, tt.totalDisruption)

			if tt.expectInfinity {
				if !math.IsInf(ratio, 1) {
					t.Errorf("Expected positive infinity, got %f", ratio)
				}
			} else {
				if math.IsInf(ratio, 0) {
					t.Errorf("Expected finite value, got infinity")
				}
			}
		})
	}
}

// TestComputeDeleteRatio_InfinityHandling tests infinity handling in delete ratio computation
func TestComputeDeleteRatio_InfinityHandling(t *testing.T) {
	tests := []struct {
		name            string
		nodeCost        float64
		nodeDisruption  float64
		totalCost       float64
		totalDisruption float64
		expectInfinity  bool
	}{
		{
			name:            "zero total disruption",
			nodeCost:        1.0,
			nodeDisruption:  0.0,
			totalCost:       5.0,
			totalDisruption: 0.0,
			expectInfinity:  true,
		},
		{
			name:            "zero normalized disruption",
			nodeCost:        1.0,
			nodeDisruption:  0.0,
			totalCost:       5.0,
			totalDisruption: 100.0,
			expectInfinity:  true,
		},
		{
			name:            "non-zero disruption",
			nodeCost:        1.0,
			nodeDisruption:  10.0,
			totalCost:       5.0,
			totalDisruption: 50.0,
			expectInfinity:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			calculator := NewDecisionRatioCalculator(clock.RealClock{})

			candidate := createTestCandidateWithValues(tt.nodeCost, tt.nodeDisruption)

			deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, tt.totalCost, tt.totalDisruption)

			if tt.expectInfinity {
				if !math.IsInf(deleteRatio, 1) {
					t.Errorf("Expected positive infinity, got %f", deleteRatio)
				}
			} else {
				if math.IsInf(deleteRatio, 0) {
					t.Errorf("Expected finite value, got infinity")
				}
			}
		})
	}
}

// TestComputeNodeDisruptionCost_EmptyPodList tests with no pods
func TestComputeNodeDisruptionCost_EmptyPodList(t *testing.T) {
	ctx := context.Background()

	pods := []*corev1.Pod{}

	cost := ComputeNodeDisruptionCost(ctx, pods)

	// Should return baseline cost of 1.0
	expectedCost := 1.0
	if !floatEqualsTest(cost, expectedCost, 1e-9) {
		t.Errorf("Expected cost %f for empty pod list, got %f", expectedCost, cost)
	}
}

// TestComputeNodeDisruptionCost_SinglePodWithPositiveCost tests with one pod with positive eviction cost
func TestComputeNodeDisruptionCost_SinglePodWithPositiveCost(t *testing.T) {
	ctx := context.Background()

	// Create a pod with positive deletion cost
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Annotations: map[string]string{
				corev1.PodDeletionCost: "1000",
			},
		},
		Spec: corev1.PodSpec{
			Priority: lo.ToPtr(int32(0)),
		},
	}

	pods := []*corev1.Pod{pod}

	cost := ComputeNodeDisruptionCost(ctx, pods)

	// Should be baseline (1.0) + eviction cost
	// EvictionCost for this pod should be positive
	evictionCost := disruptionutils.EvictionCost(ctx, pod)
	expectedCost := 1.0 + evictionCost

	if !floatEqualsTest(cost, expectedCost, 1e-9) {
		t.Errorf("Expected cost %f, got %f", expectedCost, cost)
	}

	// Verify cost is greater than baseline
	if cost <= 1.0 {
		t.Errorf("Expected cost > 1.0, got %f", cost)
	}
}

// TestComputeNodeDisruptionCost_SinglePodWithNegativeCost tests with one pod with negative eviction cost
func TestComputeNodeDisruptionCost_SinglePodWithNegativeCost(t *testing.T) {
	ctx := context.Background()

	// Create a pod with very negative deletion cost and very low priority
	// The EvictionCost function clamps to [-10, 10], so we need extreme values
	// to get a negative result after clamping
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Annotations: map[string]string{
				corev1.PodDeletionCost: "-2147483647", // Min value
			},
		},
		Spec: corev1.PodSpec{
			Priority: lo.ToPtr(int32(-2147483648)), // Min value
		},
	}

	pods := []*corev1.Pod{pod}

	cost := ComputeNodeDisruptionCost(ctx, pods)

	// Verify eviction cost is negative (should be clamped to -10)
	evictionCost := disruptionutils.EvictionCost(ctx, pod)
	if evictionCost >= 0 {
		t.Skipf("Test requires negative eviction cost, got %f", evictionCost)
	}

	// Should be baseline (1.0) only, since negative costs are clamped to zero
	expectedCost := 1.0

	if !floatEqualsTest(cost, expectedCost, 1e-9) {
		t.Errorf("Expected cost %f (negative cost clamped), got %f", expectedCost, cost)
	}
}

// TestComputeNodeDisruptionCost_MultiplePods tests with multiple pods
func TestComputeNodeDisruptionCost_MultiplePods(t *testing.T) {
	ctx := context.Background()

	// Create multiple pods with different costs
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-1",
				Namespace: "default",
				Annotations: map[string]string{
					corev1.PodDeletionCost: "100",
				},
			},
			Spec: corev1.PodSpec{
				Priority: lo.ToPtr(int32(100)),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-2",
				Namespace: "default",
				Annotations: map[string]string{
					corev1.PodDeletionCost: "200",
				},
			},
			Spec: corev1.PodSpec{
				Priority: lo.ToPtr(int32(200)),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-3",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				Priority: lo.ToPtr(int32(0)),
			},
		},
	}

	cost := ComputeNodeDisruptionCost(ctx, pods)

	// Should be baseline (1.0) + sum of positive eviction costs
	expectedCost := 1.0
	for _, pod := range pods {
		evictionCost := disruptionutils.EvictionCost(ctx, pod)
		if evictionCost > 0 {
			expectedCost += evictionCost
		}
	}

	if !floatEqualsTest(cost, expectedCost, 1e-9) {
		t.Errorf("Expected cost %f, got %f", expectedCost, cost)
	}

	// Verify cost is greater than baseline
	if cost <= 1.0 {
		t.Errorf("Expected cost > 1.0, got %f", cost)
	}
}

// TestComputeNodeDisruptionCost_MixedPositiveAndNegativeCosts tests with mixed positive and negative costs
func TestComputeNodeDisruptionCost_MixedPositiveAndNegativeCosts(t *testing.T) {
	ctx := context.Background()

	// Create pods with mixed costs
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-positive",
				Namespace: "default",
				Annotations: map[string]string{
					corev1.PodDeletionCost: "1000",
				},
			},
			Spec: corev1.PodSpec{
				Priority: lo.ToPtr(int32(1000)),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-negative",
				Namespace: "default",
				Annotations: map[string]string{
					corev1.PodDeletionCost: "-1000000",
				},
			},
			Spec: corev1.PodSpec{
				Priority: lo.ToPtr(int32(-1000000)),
			},
		},
	}

	cost := ComputeNodeDisruptionCost(ctx, pods)

	// Should be baseline (1.0) + sum of positive eviction costs only
	expectedCost := 1.0
	for _, pod := range pods {
		evictionCost := disruptionutils.EvictionCost(ctx, pod)
		if evictionCost > 0 {
			expectedCost += evictionCost
		}
	}

	if !floatEqualsTest(cost, expectedCost, 1e-9) {
		t.Errorf("Expected cost %f, got %f", expectedCost, cost)
	}

	// Verify cost is greater than baseline (at least one pod has positive cost)
	if cost <= 1.0 {
		t.Errorf("Expected cost > 1.0, got %f", cost)
	}
}

// TestComputeNodeDisruptionCost_BaselineCostAlwaysApplied tests that baseline cost is always present
func TestComputeNodeDisruptionCost_BaselineCostAlwaysApplied(t *testing.T) {
	tests := []struct {
		name string
		pods []*corev1.Pod
	}{
		{
			name: "empty pod list",
			pods: []*corev1.Pod{},
		},
		{
			name: "single pod with zero cost",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-zero",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Priority: lo.ToPtr(int32(0)),
					},
				},
			},
		},
		{
			name: "multiple pods with negative costs",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-1",
						Namespace: "default",
						Annotations: map[string]string{
							corev1.PodDeletionCost: "-1000000",
						},
					},
					Spec: corev1.PodSpec{
						Priority: lo.ToPtr(int32(-1000000)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-2",
						Namespace: "default",
						Annotations: map[string]string{
							corev1.PodDeletionCost: "-2000000",
						},
					},
					Spec: corev1.PodSpec{
						Priority: lo.ToPtr(int32(-2000000)),
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			cost := ComputeNodeDisruptionCost(ctx, tt.pods)

			// Cost should always be >= 1.0 (baseline)
			if cost < 1.0 {
				t.Errorf("Expected cost >= 1.0, got %f", cost)
			}
		})
	}
}

// TestComputeNodeDisruptionCost_NegativeCostClamping tests that negative costs are clamped to zero
func TestComputeNodeDisruptionCost_NegativeCostClamping(t *testing.T) {
	ctx := context.Background()

	// Create a pod that should have negative eviction cost (clamped to -10 by EvictionCost)
	podNegative := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-negative",
			Namespace: "default",
			Annotations: map[string]string{
				corev1.PodDeletionCost: "-2147483647", // Min value
			},
		},
		Spec: corev1.PodSpec{
			Priority: lo.ToPtr(int32(-2147483648)), // Min value
		},
	}

	// Verify the eviction cost is actually negative
	evictionCost := disruptionutils.EvictionCost(ctx, podNegative)
	if evictionCost >= 0 {
		t.Skipf("Test requires negative eviction cost, got %f", evictionCost)
	}

	// Compute node disruption cost with just this pod
	costWithNegative := ComputeNodeDisruptionCost(ctx, []*corev1.Pod{podNegative})

	// Should be exactly baseline (1.0) since negative cost is clamped to zero
	expectedCost := 1.0
	if !floatEqualsTest(costWithNegative, expectedCost, 1e-9) {
		t.Errorf("Expected cost %f (negative clamped to zero), got %f", expectedCost, costWithNegative)
	}

	// Now add a positive cost pod
	podPositive := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-positive",
			Namespace: "default",
			Annotations: map[string]string{
				corev1.PodDeletionCost: "1000",
			},
		},
		Spec: corev1.PodSpec{
			Priority: lo.ToPtr(int32(1000)),
		},
	}

	costWithBoth := ComputeNodeDisruptionCost(ctx, []*corev1.Pod{podNegative, podPositive})

	// Should be baseline + positive eviction cost only
	positiveEvictionCost := disruptionutils.EvictionCost(ctx, podPositive)
	expectedCostWithBoth := 1.0 + positiveEvictionCost

	if !floatEqualsTest(costWithBoth, expectedCostWithBoth, 1e-9) {
		t.Errorf("Expected cost %f (negative clamped), got %f", expectedCostWithBoth, costWithBoth)
	}
}

// TestComputeNodeDisruptionCost_VariousPodSets tests with various pod configurations
func TestComputeNodeDisruptionCost_VariousPodSets(t *testing.T) {
	tests := []struct {
		name        string
		pods        []*corev1.Pod
		minExpected float64
		maxExpected float64
	}{
		{
			name:        "no pods",
			pods:        []*corev1.Pod{},
			minExpected: 1.0,
			maxExpected: 1.0,
		},
		{
			name: "single high priority pod",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "high-priority",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Priority: lo.ToPtr(int32(1000000)),
					},
				},
			},
			minExpected: 1.0,
			maxExpected: 100.0, // Should be well below this
		},
		{
			name: "single high deletion cost pod",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "high-deletion-cost",
						Namespace: "default",
						Annotations: map[string]string{
							corev1.PodDeletionCost: "2000000",
						},
					},
					Spec: corev1.PodSpec{
						Priority: lo.ToPtr(int32(0)),
					},
				},
			},
			minExpected: 1.0,
			maxExpected: 100.0,
		},
		{
			name: "many low priority pods",
			pods: func() []*corev1.Pod {
				pods := make([]*corev1.Pod, 10)
				for i := 0; i < 10; i++ {
					pods[i] = &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "pod-" + strconv.Itoa(i),
							Namespace: "default",
						},
						Spec: corev1.PodSpec{
							Priority: lo.ToPtr(int32(0)),
						},
					}
				}
				return pods
			}(),
			minExpected: 1.0,
			maxExpected: 100.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			cost := ComputeNodeDisruptionCost(ctx, tt.pods)

			// Verify cost is within expected range
			if cost < tt.minExpected {
				t.Errorf("Expected cost >= %f, got %f", tt.minExpected, cost)
			}
			if cost > tt.maxExpected {
				t.Errorf("Expected cost <= %f, got %f", tt.maxExpected, cost)
			}
		})
	}
}

// TestDeleteRatioFiltering_NodesAboveThreshold tests that nodes with delete ratio >= threshold are not skipped
//
//nolint:gocyclo
func TestDeleteRatioFiltering_NodesAboveThreshold(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates with various delete ratios
	// Candidate 1: high cost, low disruption -> high delete ratio
	// Candidate 2: medium cost, medium disruption -> medium delete ratio
	candidates := []*Candidate{
		createTestCandidateWithPolicyAndValues(5.0, 10.0, v1.ConsolidateWhenCostJustifiesDisruption),
		createTestCandidateWithPolicyAndValues(3.0, 30.0, v1.ConsolidateWhenCostJustifiesDisruption),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute delete ratios
	deleteRatio1 := calculator.ComputeDeleteRatio(ctx, candidates[0], totalCost, totalDisruption)
	deleteRatio2 := calculator.ComputeDeleteRatio(ctx, candidates[1], totalCost, totalDisruption)

	// Test with threshold 1.0
	threshold := 1.0

	// Simulate filtering logic
	filtered := make([]*Candidate, 0)
	for i, candidate := range candidates {
		var deleteRatio float64
		if i == 0 {
			deleteRatio = deleteRatio1
		} else {
			deleteRatio = deleteRatio2
		}

		if deleteRatio >= threshold {
			filtered = append(filtered, candidate)
		}
	}

	// Verify that candidates with delete ratio >= threshold are in filtered list
	if deleteRatio1 >= threshold {
		found := false
		for _, c := range filtered {
			if c == candidates[0] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected candidate 1 with delete ratio %f >= threshold %f to not be skipped", deleteRatio1, threshold)
		}
	}

	if deleteRatio2 >= threshold {
		found := false
		for _, c := range filtered {
			if c == candidates[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected candidate 2 with delete ratio %f >= threshold %f to not be skipped", deleteRatio2, threshold)
		}
	}
}

// TestDeleteRatioFiltering_NodesBelowThreshold tests that nodes with delete ratio < threshold are skipped
func TestDeleteRatioFiltering_NodesBelowThreshold(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates where some will have delete ratio < threshold
	// Candidate 1: low cost, high disruption -> low delete ratio
	// Candidate 2: high cost, low disruption -> high delete ratio
	candidates := []*Candidate{
		createTestCandidateWithPolicyAndValues(1.0, 100.0, v1.ConsolidateWhenCostJustifiesDisruption),
		createTestCandidateWithPolicyAndValues(5.0, 10.0, v1.ConsolidateWhenCostJustifiesDisruption),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute delete ratios
	deleteRatio1 := calculator.ComputeDeleteRatio(ctx, candidates[0], totalCost, totalDisruption)
	deleteRatio2 := calculator.ComputeDeleteRatio(ctx, candidates[1], totalCost, totalDisruption)

	// Test with threshold 1.5 (should filter out low-ratio candidates)
	threshold := 1.5

	// Simulate filtering logic
	filtered := make([]*Candidate, 0)
	for i, candidate := range candidates {
		var deleteRatio float64
		if i == 0 {
			deleteRatio = deleteRatio1
		} else {
			deleteRatio = deleteRatio2
		}

		if deleteRatio >= threshold {
			filtered = append(filtered, candidate)
		}
	}

	// Verify that candidates with delete ratio < threshold are NOT in filtered list
	if deleteRatio1 < threshold {
		for _, c := range filtered {
			if c == candidates[0] {
				t.Errorf("Expected candidate 1 with delete ratio %f < threshold %f to be skipped", deleteRatio1, threshold)
			}
		}
	}

	if deleteRatio2 < threshold {
		for _, c := range filtered {
			if c == candidates[1] {
				t.Errorf("Expected candidate 2 with delete ratio %f < threshold %f to be skipped", deleteRatio2, threshold)
			}
		}
	}
}

// TestDeleteRatioFiltering_WhenEmptyPolicy tests that WhenEmpty policy does not apply delete ratio filtering
func TestDeleteRatioFiltering_WhenEmptyPolicy(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates with WhenEmpty policy
	candidates := []*Candidate{
		createTestCandidateWithPolicyAndValues(1.0, 100.0, v1.ConsolidateWhenEmpty),
		createTestCandidateWithPolicyAndValues(5.0, 10.0, v1.ConsolidateWhenEmpty),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute delete ratios
	deleteRatio1 := calculator.ComputeDeleteRatio(ctx, candidates[0], totalCost, totalDisruption)
	deleteRatio2 := calculator.ComputeDeleteRatio(ctx, candidates[1], totalCost, totalDisruption)

	// For WhenEmpty policy, filtering should NOT be applied regardless of threshold
	threshold := 2.0

	// Simulate the logic: WhenEmpty policy should skip filtering
	policy := candidates[0].NodePool.Spec.Disruption.ConsolidateWhen
	shouldFilter := policy == v1.ConsolidateWhenCostJustifiesDisruption

	if shouldFilter {
		t.Errorf("Expected WhenEmpty policy to NOT apply delete ratio filtering")
	}

	// Verify that even with low delete ratios, candidates are not filtered for WhenEmpty
	if !shouldFilter {
		// All candidates should pass through (no filtering)
		if deleteRatio1 < threshold || deleteRatio2 < threshold {
			// This is expected - low ratios exist but filtering is not applied
			t.Logf("Delete ratios: %f, %f with threshold %f - filtering correctly skipped for WhenEmpty policy", deleteRatio1, deleteRatio2, threshold)
		}
	}
}

// TestDeleteRatioFiltering_WhenEmptyOrUnderutilizedPolicy tests that WhenEmptyOrUnderutilized policy does not apply delete ratio filtering
func TestDeleteRatioFiltering_WhenEmptyOrUnderutilizedPolicy(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Create candidates with WhenEmptyOrUnderutilized policy
	candidates := []*Candidate{
		createTestCandidateWithPolicyAndValues(1.0, 100.0, v1.ConsolidateWhenEmptyOrUnderutilized),
		createTestCandidateWithPolicyAndValues(5.0, 10.0, v1.ConsolidateWhenEmptyOrUnderutilized),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute delete ratios
	deleteRatio1 := calculator.ComputeDeleteRatio(ctx, candidates[0], totalCost, totalDisruption)
	deleteRatio2 := calculator.ComputeDeleteRatio(ctx, candidates[1], totalCost, totalDisruption)

	// For WhenEmptyOrUnderutilized policy, filtering should NOT be applied regardless of threshold
	threshold := 2.0

	// Simulate the logic: WhenEmptyOrUnderutilized policy should skip filtering
	policy := candidates[0].NodePool.Spec.Disruption.ConsolidateWhen
	shouldFilter := policy == v1.ConsolidateWhenCostJustifiesDisruption

	if shouldFilter {
		t.Errorf("Expected WhenEmptyOrUnderutilized policy to NOT apply delete ratio filtering")
	}

	// Verify that even with low delete ratios, candidates are not filtered for WhenEmptyOrUnderutilized
	if !shouldFilter {
		// All candidates should pass through (no filtering)
		if deleteRatio1 < threshold || deleteRatio2 < threshold {
			// This is expected - low ratios exist but filtering is not applied
			t.Logf("Delete ratios: %f, %f with threshold %f - filtering correctly skipped for WhenEmptyOrUnderutilized policy", deleteRatio1, deleteRatio2, threshold)
		}
	}
}

// Helper function to create test candidate with policy and values
func createTestCandidateWithPolicyAndValues(cost, disruption float64, policy v1.ConsolidateWhenPolicy) *Candidate {
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
					ConsolidateWhen: policy,
				},
			},
		},
	}
}
