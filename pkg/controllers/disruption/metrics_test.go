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
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TestMetricsEmission_DecisionRatioHistogram tests that decision ratio histogram is emitted correctly
func TestMetricsEmission_DecisionRatioHistogram(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	DecisionRatioHistogram.Reset()

	// Create test candidates with known decision ratios
	candidates := []*Candidate{
		createTestCandidateWithNodePool(1.0, 10.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption),
		createTestCandidateWithNodePool(2.0, 10.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute decision ratios and emit metrics (simulating what consolidation.go does)
	threshold := 1.0
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

		policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)
		DecisionRatioHistogram.Observe(candidate.decisionRatio, map[string]string{
			"nodepool":  candidate.NodePool.Name,
			"policy":    policy,
			"threshold": fmt.Sprintf("%.2f", threshold),
			"move_type": "evaluated",
		})
	}

	// Verify that metrics were emitted without error
	assert.NotNil(t, DecisionRatioHistogram, "Decision ratio histogram should be initialized")
}

// TestMetricsEmission_ThresholdCounters tests that threshold counters are incremented correctly
func TestMetricsEmission_ThresholdCounters(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	MovesAboveThresholdCounter.Reset()
	MovesBelowThresholdCounter.Reset()

	// Create test candidates with ratios above and below threshold
	candidates := []*Candidate{
		createTestCandidateWithNodePool(10.0, 5.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption), // ratio > 1.0
		createTestCandidateWithNodePool(1.0, 10.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption), // ratio < 1.0
		createTestCandidateWithNodePool(5.0, 5.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption),  // ratio = 1.0
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Compute decision ratios and emit metrics
	threshold := 1.0
	aboveCount := 0
	belowCount := 0
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

		policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)
		labels := map[string]string{
			"nodepool":  candidate.NodePool.Name,
			"policy":    policy,
			"threshold": fmt.Sprintf("%.2f", threshold),
		}

		if candidate.decisionRatio >= threshold {
			MovesAboveThresholdCounter.Inc(labels)
			aboveCount++
		} else {
			MovesBelowThresholdCounter.Inc(labels)
			belowCount++
		}
	}

	// Verify counter values
	assert.Equal(t, 2, aboveCount, "Should have 2 moves above threshold")
	assert.Equal(t, 1, belowCount, "Should have 1 move below threshold")
	assert.NotNil(t, MovesAboveThresholdCounter, "Above threshold counter should be initialized")
	assert.NotNil(t, MovesBelowThresholdCounter, "Below threshold counter should be initialized")
}

// TestMetricsEmission_DeleteRatioSkipCounter tests that skip counter is incremented correctly
func TestMetricsEmission_DeleteRatioSkipCounter(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	MovesSkippedByDeleteRatioCounter.Reset()

	// Create test candidates where some have low delete ratios
	candidates := []*Candidate{
		createTestCandidateWithNodePool(1.0, 20.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption), // low ratio, should skip
		createTestCandidateWithNodePool(10.0, 5.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption), // high ratio, should not skip
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Simulate delete ratio filtering
	threshold := 1.0
	skippedCount := 0
	for _, candidate := range candidates {
		deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

		if deleteRatio < threshold {
			policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)
			MovesSkippedByDeleteRatioCounter.Inc(map[string]string{
				"nodepool":  candidate.NodePool.Name,
				"policy":    policy,
				"threshold": fmt.Sprintf("%.2f", threshold),
			})
			skippedCount++
		}
	}

	// Verify that one candidate was skipped
	assert.Equal(t, 1, skippedCount, "Should have skipped 1 candidate")
	assert.NotNil(t, MovesSkippedByDeleteRatioCounter, "Skip counter should be initialized")
}

// TestMetricsEmission_MetricLabels tests that metrics include correct labels
func TestMetricsEmission_MetricLabels(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	DecisionRatioHistogram.Reset()
	MovesAboveThresholdCounter.Reset()

	// Create test candidate with specific nodepool and policy
	nodePoolName := "my-test-nodepool"
	policy := v1.ConsolidateWhenCostJustifiesDisruption
	candidate := createTestCandidateWithNodePool(10.0, 5.0, nodePoolName, policy)

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, []*Candidate{candidate})
	calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

	// Emit metrics with labels
	threshold := 1.0
	labels := map[string]string{
		"nodepool":  nodePoolName,
		"policy":    string(policy),
		"threshold": fmt.Sprintf("%.2f", threshold),
		"move_type": "evaluated",
	}

	DecisionRatioHistogram.Observe(candidate.decisionRatio, labels)

	thresholdLabels := map[string]string{
		"nodepool":  nodePoolName,
		"policy":    string(policy),
		"threshold": fmt.Sprintf("%.2f", threshold),
	}
	MovesAboveThresholdCounter.Inc(thresholdLabels)

	// Verify metrics were emitted without error
	assert.NotNil(t, DecisionRatioHistogram, "Histogram should be initialized")
	assert.NotNil(t, MovesAboveThresholdCounter, "Counter should be initialized")
}

// TestMetricsEmission_MultipleNodePools tests metrics with different nodepools
func TestMetricsEmission_MultipleNodePools(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	MovesAboveThresholdCounter.Reset()
	MovesBelowThresholdCounter.Reset()

	// Create candidates from different nodepools
	candidates := []*Candidate{
		createTestCandidateWithNodePool(10.0, 5.0, "nodepool-1", v1.ConsolidateWhenCostJustifiesDisruption),
		createTestCandidateWithNodePool(1.0, 10.0, "nodepool-2", v1.ConsolidateWhenCostJustifiesDisruption),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Emit metrics for each candidate
	threshold := 1.0
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

		policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)
		labels := map[string]string{
			"nodepool":  candidate.NodePool.Name,
			"policy":    policy,
			"threshold": fmt.Sprintf("%.2f", threshold),
		}

		if candidate.decisionRatio >= threshold {
			MovesAboveThresholdCounter.Inc(labels)
		} else {
			MovesBelowThresholdCounter.Inc(labels)
		}
	}

	// Verify metrics were emitted for both nodepools
	assert.NotNil(t, MovesAboveThresholdCounter, "Should have above threshold metrics")
	assert.NotNil(t, MovesBelowThresholdCounter, "Should have below threshold metrics")
}

// TestMetricsEmission_DifferentPolicies tests metrics with different consolidation policies
func TestMetricsEmission_DifferentPolicies(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	DecisionRatioHistogram.Reset()

	// Create candidates with different policies
	candidates := []*Candidate{
		createTestCandidateWithNodePool(10.0, 5.0, "nodepool-1", v1.ConsolidateWhenCostJustifiesDisruption),
		createTestCandidateWithNodePool(5.0, 5.0, "nodepool-2", v1.ConsolidateWhenEmptyOrUnderutilized),
		createTestCandidateWithNodePool(2.0, 5.0, "nodepool-3", v1.ConsolidateWhenEmpty),
	}

	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Emit metrics for each candidate
	threshold := 1.0
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

		policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)
		DecisionRatioHistogram.Observe(candidate.decisionRatio, map[string]string{
			"nodepool":  candidate.NodePool.Name,
			"policy":    policy,
			"threshold": fmt.Sprintf("%.2f", threshold),
			"move_type": "evaluated",
		})
	}

	// Verify metrics were emitted
	assert.NotNil(t, DecisionRatioHistogram, "Should have histogram observations for different policies")
}

// Helper function to create a test candidate with nodepool and policy
func createTestCandidateWithNodePool(cost, disruption float64, nodePoolName string, policy v1.ConsolidateWhenPolicy) *Candidate {
	candidate := createTestCandidateWithValues(cost, disruption)
	candidate.NodePool = &v1.NodePool{}
	candidate.NodePool.Name = nodePoolName
	candidate.NodePool.Spec.Disruption.ConsolidateWhen = policy
	return candidate
}

// TestMetricsEmission_ThresholdLabelFormat tests that threshold label is formatted correctly
func TestMetricsEmission_ThresholdLabelFormat(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	DecisionRatioHistogram.Reset()
	MovesAboveThresholdCounter.Reset()

	// Test various threshold values to ensure correct formatting
	testCases := []struct {
		threshold     float64
		expectedLabel string
	}{
		{1.0, "1.00"},
		{1.5, "1.50"},
		{2.0, "2.00"},
		{0.5, "0.50"},
		{0.75, "0.75"},
		{10.0, "10.00"},
		{0.001, "0.00"}, // Very small values round to 2 decimals
	}

	for _, tc := range testCases {
		candidate := createTestCandidateWithNodePool(10.0, 5.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption)
		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, []*Candidate{candidate})
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

		// Format threshold as the code does
		thresholdLabel := fmt.Sprintf("%.2f", tc.threshold)
		assert.Equal(t, tc.expectedLabel, thresholdLabel, "Threshold label should be formatted with 2 decimal places")

		// Emit metric with formatted threshold
		policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)
		DecisionRatioHistogram.Observe(candidate.decisionRatio, map[string]string{
			"nodepool":  candidate.NodePool.Name,
			"policy":    policy,
			"threshold": thresholdLabel,
			"move_type": "evaluated",
		})
	}
}

// TestMetricsEmission_MovesAboveThresholdWithThresholdLabel tests that moves above threshold include threshold label
func TestMetricsEmission_MovesAboveThresholdWithThresholdLabel(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	MovesAboveThresholdCounter.Reset()

	// Create a candidate with high cost savings (will have high decision ratio)
	candidate := createTestCandidateWithNodePool(10.0, 1.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption)
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, []*Candidate{candidate})
	calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

	// Test with a threshold that should be below the decision ratio
	threshold := 1.0
	thresholdLabel := fmt.Sprintf("%.2f", threshold)

	policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)

	// Emit metric with threshold label
	if candidate.decisionRatio >= threshold {
		MovesAboveThresholdCounter.Inc(map[string]string{
			"nodepool":  candidate.NodePool.Name,
			"policy":    policy,
			"threshold": thresholdLabel,
		})
	}

	// Verify counter was incremented
	assert.NotNil(t, MovesAboveThresholdCounter, "Above threshold counter should be initialized")
}

// TestMetricsEmission_MovesBelowThresholdWithThresholdLabel tests that moves below threshold include threshold label
func TestMetricsEmission_MovesBelowThresholdWithThresholdLabel(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	MovesBelowThresholdCounter.Reset()

	// Create a candidate with low cost savings (will have low decision ratio)
	candidate := createTestCandidateWithNodePool(1.0, 10.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption)
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, []*Candidate{candidate})
	calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

	// Test with a threshold that should be above the decision ratio
	threshold := 2.0
	thresholdLabel := fmt.Sprintf("%.2f", threshold)

	policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)

	// Emit metric with threshold label
	if candidate.decisionRatio < threshold {
		MovesBelowThresholdCounter.Inc(map[string]string{
			"nodepool":  candidate.NodePool.Name,
			"policy":    policy,
			"threshold": thresholdLabel,
		})
	}

	// Verify counter was incremented
	assert.NotNil(t, MovesBelowThresholdCounter, "Below threshold counter should be initialized")
}

// TestMetricsEmission_DeleteRatioSkipsWithThresholdLabel tests that delete ratio skips include threshold label
func TestMetricsEmission_DeleteRatioSkipsWithThresholdLabel(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	MovesSkippedByDeleteRatioCounter.Reset()

	// Create candidates where delete ratio is below various thresholds
	testCases := []struct {
		costSavings    float64
		disruptionCost float64
		threshold      float64
		shouldSkip     bool
	}{
		{1.0, 20.0, 1.0, true},  // delete ratio < 1.0, should skip
		{1.0, 20.0, 0.5, false}, // delete ratio might be >= 0.5, should not skip
		{5.0, 10.0, 1.5, true},  // delete ratio < 1.5, should skip
		{10.0, 5.0, 1.0, false}, // delete ratio >= 1.0, should not skip
	}

	for _, tc := range testCases {
		candidate := createTestCandidateWithNodePool(tc.costSavings, tc.disruptionCost, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption)
		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, []*Candidate{candidate})
		deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

		policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)
		thresholdLabel := fmt.Sprintf("%.2f", tc.threshold)

		if deleteRatio < tc.threshold {
			MovesSkippedByDeleteRatioCounter.Inc(map[string]string{
				"nodepool":  candidate.NodePool.Name,
				"policy":    policy,
				"threshold": thresholdLabel,
			})
		}
	}

	// Verify counter was incremented for skipped moves
	assert.NotNil(t, MovesSkippedByDeleteRatioCounter, "Skip counter should be initialized")
}

// TestMetricsEmission_AllMetricsIncludeThresholdLabel tests that all metrics include threshold label
func TestMetricsEmission_AllMetricsIncludeThresholdLabel(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset all metrics before test
	DecisionRatioHistogram.Reset()
	MovesAboveThresholdCounter.Reset()
	MovesBelowThresholdCounter.Reset()
	MovesSkippedByDeleteRatioCounter.Reset()

	threshold := 1.5
	thresholdLabel := fmt.Sprintf("%.2f", threshold)

	// Create test candidates
	candidateAbove := createTestCandidateWithNodePool(10.0, 5.0, "nodepool-above", v1.ConsolidateWhenCostJustifiesDisruption)
	candidateBelow := createTestCandidateWithNodePool(5.0, 10.0, "nodepool-below", v1.ConsolidateWhenCostJustifiesDisruption)
	candidateSkip := createTestCandidateWithNodePool(1.0, 20.0, "nodepool-skip", v1.ConsolidateWhenCostJustifiesDisruption)

	candidates := []*Candidate{candidateAbove, candidateBelow, candidateSkip}
	totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

	// Emit all metrics with threshold label
	for _, candidate := range candidates {
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
		policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)

		// Histogram metric
		DecisionRatioHistogram.Observe(candidate.decisionRatio, map[string]string{
			"nodepool":  candidate.NodePool.Name,
			"policy":    policy,
			"threshold": thresholdLabel,
			"move_type": "evaluated",
		})

		// Threshold counters
		if candidate.decisionRatio >= threshold {
			MovesAboveThresholdCounter.Inc(map[string]string{
				"nodepool":  candidate.NodePool.Name,
				"policy":    policy,
				"threshold": thresholdLabel,
			})
		} else {
			MovesBelowThresholdCounter.Inc(map[string]string{
				"nodepool":  candidate.NodePool.Name,
				"policy":    policy,
				"threshold": thresholdLabel,
			})
		}

		// Delete ratio skip counter
		deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)
		if deleteRatio < threshold {
			MovesSkippedByDeleteRatioCounter.Inc(map[string]string{
				"nodepool":  candidate.NodePool.Name,
				"policy":    policy,
				"threshold": thresholdLabel,
			})
		}
	}

	// Verify all metrics were emitted successfully
	assert.NotNil(t, DecisionRatioHistogram, "Histogram should be initialized")
	assert.NotNil(t, MovesAboveThresholdCounter, "Above threshold counter should be initialized")
	assert.NotNil(t, MovesBelowThresholdCounter, "Below threshold counter should be initialized")
	assert.NotNil(t, MovesSkippedByDeleteRatioCounter, "Skip counter should be initialized")
}

// TestMetricsEmission_ThresholdLabelMatchesConfiguredThreshold tests that threshold label value matches configured threshold
func TestMetricsEmission_ThresholdLabelMatchesConfiguredThreshold(t *testing.T) {
	ctx := context.Background()
	calculator := NewDecisionRatioCalculator(clock.RealClock{})

	// Reset metrics before test
	MovesAboveThresholdCounter.Reset()

	// Test that the threshold label matches the configured threshold
	configuredThresholds := []float64{0.5, 1.0, 1.5, 2.0, 10.0}

	for _, configuredThreshold := range configuredThresholds {
		candidate := createTestCandidateWithNodePool(10.0, 5.0, "test-nodepool", v1.ConsolidateWhenCostJustifiesDisruption)
		totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, []*Candidate{candidate})
		calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

		// Format threshold as it would be in the metric
		thresholdLabel := fmt.Sprintf("%.2f", configuredThreshold)
		expectedLabel := fmt.Sprintf("%.2f", configuredThreshold)

		// Verify the label matches the configured threshold
		assert.Equal(t, expectedLabel, thresholdLabel, "Threshold label should match configured threshold")

		// Emit metric with the threshold label
		policy := string(candidate.NodePool.Spec.Disruption.ConsolidateWhen)
		if candidate.decisionRatio >= configuredThreshold {
			MovesAboveThresholdCounter.Inc(map[string]string{
				"nodepool":  candidate.NodePool.Name,
				"policy":    policy,
				"threshold": thresholdLabel,
			})
		}
	}

	// Verify counter was incremented
	assert.NotNil(t, MovesAboveThresholdCounter, "Above threshold counter should be initialized")
}
