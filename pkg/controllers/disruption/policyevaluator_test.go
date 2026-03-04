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
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TestPolicyEvaluator_WhenEmpty tests the WhenEmpty policy behavior
func TestPolicyEvaluator_WhenEmpty(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		podCount      int
		decisionRatio float64
		expected      bool
	}{
		{
			name:          "empty node with ratio below threshold",
			podCount:      0,
			decisionRatio: 0.5,
			expected:      true, // Should consolidate because node is empty
		},
		{
			name:          "empty node with ratio above threshold",
			podCount:      0,
			decisionRatio: 2.0,
			expected:      true, // Should consolidate because node is empty
		},
		{
			name:          "empty node with ratio at threshold",
			podCount:      0,
			decisionRatio: 1.0,
			expected:      true, // Should consolidate because node is empty
		},
		{
			name:          "non-empty node with ratio above threshold",
			podCount:      5,
			decisionRatio: 2.0,
			expected:      false, // Should not consolidate because node has pods
		},
		{
			name:          "non-empty node with ratio below threshold",
			podCount:      5,
			decisionRatio: 0.5,
			expected:      false, // Should not consolidate because node has pods
		},
		{
			name:          "single pod with high ratio",
			podCount:      1,
			decisionRatio: 10.0,
			expected:      false, // Should not consolidate because node has pods
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(tt.podCount)
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenEmpty,
				candidate,
				tt.decisionRatio,
				1.0, // threshold parameter (ignored for WhenEmpty policy)
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestPolicyEvaluator_WhenEmptyOrUnderutilized tests the WhenEmptyOrUnderutilized policy behavior
func TestPolicyEvaluator_WhenEmptyOrUnderutilized(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		podCount      int
		decisionRatio float64
		expected      bool
	}{
		{
			name:          "empty node with low ratio",
			podCount:      0,
			decisionRatio: 0.1,
			expected:      true, // Should always consolidate
		},
		{
			name:          "empty node with high ratio",
			podCount:      0,
			decisionRatio: 10.0,
			expected:      true, // Should always consolidate
		},
		{
			name:          "non-empty node with low ratio",
			podCount:      10,
			decisionRatio: 0.1,
			expected:      true, // Should always consolidate
		},
		{
			name:          "non-empty node with high ratio",
			podCount:      10,
			decisionRatio: 10.0,
			expected:      true, // Should always consolidate
		},
		{
			name:          "node with ratio at threshold",
			podCount:      5,
			decisionRatio: 1.0,
			expected:      true, // Should always consolidate
		},
		{
			name:          "node with zero ratio",
			podCount:      5,
			decisionRatio: 0.0,
			expected:      true, // Should always consolidate
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(tt.podCount)
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenEmptyOrUnderutilized,
				candidate,
				tt.decisionRatio,
				1.0, // threshold parameter (ignored for WhenEmptyOrUnderutilized policy)
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestPolicyEvaluator_WhenCostJustifiesDisruption tests the WhenCostJustifiesDisruption policy behavior
func TestPolicyEvaluator_WhenCostJustifiesDisruption(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		podCount      int
		decisionRatio float64
		expected      bool
	}{
		{
			name:          "ratio above threshold",
			podCount:      5,
			decisionRatio: 2.0,
			expected:      true, // Should consolidate because ratio >= 1.0
		},
		{
			name:          "ratio at threshold",
			podCount:      5,
			decisionRatio: 1.0,
			expected:      true, // Should consolidate because ratio >= 1.0
		},
		{
			name:          "ratio below threshold",
			podCount:      5,
			decisionRatio: 0.5,
			expected:      false, // Should not consolidate because ratio < 1.0
		},
		{
			name:          "ratio slightly below threshold",
			podCount:      5,
			decisionRatio: 0.99,
			expected:      false, // Should not consolidate because ratio < 1.0
		},
		{
			name:          "ratio slightly above threshold",
			podCount:      5,
			decisionRatio: 1.01,
			expected:      true, // Should consolidate because ratio >= 1.0
		},
		{
			name:          "empty node with low ratio",
			podCount:      0,
			decisionRatio: 0.5,
			expected:      false, // Should not consolidate because ratio < 1.0 (policy ignores pod count)
		},
		{
			name:          "empty node with high ratio",
			podCount:      0,
			decisionRatio: 2.0,
			expected:      true, // Should consolidate because ratio >= 1.0
		},
		{
			name:          "zero ratio",
			podCount:      5,
			decisionRatio: 0.0,
			expected:      false, // Should not consolidate because ratio < 1.0
		},
		{
			name:          "very high ratio",
			podCount:      5,
			decisionRatio: 100.0,
			expected:      true, // Should consolidate because ratio >= 1.0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(tt.podCount)
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenCostJustifiesDisruption,
				candidate,
				tt.decisionRatio,
				1.0, // threshold parameter (using default 1.0)
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestPolicyEvaluator_DefaultPolicy tests the default policy behavior (unknown policy)
func TestPolicyEvaluator_DefaultPolicy(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		policy        v1.ConsolidateWhenPolicy
		podCount      int
		decisionRatio float64
		expected      bool
	}{
		{
			name:          "unknown policy with low ratio",
			policy:        v1.ConsolidateWhenPolicy("UnknownPolicy"),
			podCount:      5,
			decisionRatio: 0.5,
			expected:      true, // Should default to allowing consolidation
		},
		{
			name:          "unknown policy with high ratio",
			policy:        v1.ConsolidateWhenPolicy("UnknownPolicy"),
			podCount:      5,
			decisionRatio: 2.0,
			expected:      true, // Should default to allowing consolidation
		},
		{
			name:          "empty policy string with low ratio",
			policy:        v1.ConsolidateWhenPolicy(""),
			podCount:      5,
			decisionRatio: 0.5,
			expected:      true, // Should default to allowing consolidation
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(tt.podCount)
			result := evaluator.ShouldConsolidate(
				tt.policy,
				candidate,
				tt.decisionRatio,
				1.0, // threshold parameter
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestPolicyEvaluator_EdgeCases tests edge cases for all policies
func TestPolicyEvaluator_EdgeCases(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		policy        v1.ConsolidateWhenPolicy
		podCount      int
		decisionRatio float64
		expected      bool
	}{
		{
			name:          "WhenEmpty with exactly zero pods",
			policy:        v1.ConsolidateWhenEmpty,
			podCount:      0,
			decisionRatio: 0.5,
			expected:      true,
		},
		{
			name:          "WhenEmpty with exactly one pod",
			policy:        v1.ConsolidateWhenEmpty,
			podCount:      1,
			decisionRatio: 10.0,
			expected:      false,
		},
		{
			name:          "WhenCostJustifiesDisruption with ratio exactly 1.0",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 1.0,
			expected:      true,
		},
		{
			name:          "WhenCostJustifiesDisruption with ratio just below 1.0",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 0.9999999,
			expected:      false,
		},
		{
			name:          "WhenCostJustifiesDisruption with ratio just above 1.0",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 1.0000001,
			expected:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(tt.podCount)
			result := evaluator.ShouldConsolidate(
				tt.policy,
				candidate,
				tt.decisionRatio,
				1.0, // threshold parameter
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// createCandidateWithPods creates a test candidate with the specified number of reschedulable pods
func createCandidateWithPods(podCount int) *Candidate {
	pods := make([]*corev1.Pod, podCount)
	for i := 0; i < podCount; i++ {
		pods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
		}
	}

	return &Candidate{
		reschedulablePods: pods,
	}
}

// TestPolicyEvaluator_ConfigurableThreshold tests PolicyEvaluator with specific threshold values
// Validates: Requirements 2.1, 2.2, 2.3, 12.1
func TestPolicyEvaluator_ConfigurableThreshold(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		policy        v1.ConsolidateWhenPolicy
		podCount      int
		decisionRatio float64
		threshold     float64
		expected      bool
		description   string
	}{
		{
			name:          "WhenCostJustifiesDisruption with threshold 1.5 and ratio 2.0 should consolidate",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 2.0,
			threshold:     1.5,
			expected:      true,
			description:   "Ratio 2.0 >= threshold 1.5, should consolidate",
		},
		{
			name:          "WhenCostJustifiesDisruption with threshold 1.5 and ratio 1.0 should not consolidate",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 1.0,
			threshold:     1.5,
			expected:      false,
			description:   "Ratio 1.0 < threshold 1.5, should not consolidate",
		},
		{
			name:          "WhenCostJustifiesDisruption with threshold 1.0 and ratio 1.0 should consolidate (boundary case)",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 1.0,
			threshold:     1.0,
			expected:      true,
			description:   "Ratio 1.0 == threshold 1.0, should consolidate (boundary case)",
		},
		{
			name:          "WhenEmpty policy ignores threshold - high threshold with low ratio",
			policy:        v1.ConsolidateWhenEmpty,
			podCount:      0,
			decisionRatio: 0.5,
			threshold:     2.0,
			expected:      true,
			description:   "WhenEmpty policy should consolidate empty nodes regardless of threshold",
		},
		{
			name:          "WhenEmpty policy ignores threshold - low threshold with high ratio",
			policy:        v1.ConsolidateWhenEmpty,
			podCount:      0,
			decisionRatio: 3.0,
			threshold:     0.5,
			expected:      true,
			description:   "WhenEmpty policy should consolidate empty nodes regardless of threshold",
		},
		{
			name:          "WhenEmpty policy ignores threshold - non-empty node",
			policy:        v1.ConsolidateWhenEmpty,
			podCount:      5,
			decisionRatio: 10.0,
			threshold:     0.1,
			expected:      false,
			description:   "WhenEmpty policy should not consolidate non-empty nodes regardless of threshold",
		},
		{
			name:          "WhenEmptyOrUnderutilized policy ignores threshold - low ratio high threshold",
			policy:        v1.ConsolidateWhenEmptyOrUnderutilized,
			podCount:      5,
			decisionRatio: 0.1,
			threshold:     2.0,
			expected:      true,
			description:   "WhenEmptyOrUnderutilized policy should always consolidate regardless of threshold",
		},
		{
			name:          "WhenEmptyOrUnderutilized policy ignores threshold - high ratio low threshold",
			policy:        v1.ConsolidateWhenEmptyOrUnderutilized,
			podCount:      5,
			decisionRatio: 5.0,
			threshold:     0.5,
			expected:      true,
			description:   "WhenEmptyOrUnderutilized policy should always consolidate regardless of threshold",
		},
		{
			name:          "Conservative threshold 2.0 with ratio 1.5 should not consolidate",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 1.5,
			threshold:     2.0,
			expected:      false,
			description:   "Conservative threshold requires higher cost savings",
		},
		{
			name:          "Aggressive threshold 0.5 with ratio 0.7 should consolidate",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 0.7,
			threshold:     0.5,
			expected:      true,
			description:   "Aggressive threshold allows consolidation with lower cost savings",
		},
		{
			name:          "Aggressive threshold 0.5 with ratio 0.4 should not consolidate",
			policy:        v1.ConsolidateWhenCostJustifiesDisruption,
			podCount:      5,
			decisionRatio: 0.4,
			threshold:     0.5,
			expected:      false,
			description:   "Even aggressive threshold has a minimum requirement",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(tt.podCount)
			result := evaluator.ShouldConsolidate(
				tt.policy,
				candidate,
				tt.decisionRatio,
				tt.threshold,
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v. %s", result, tt.expected, tt.description)
			}
		})
	}
}

// TestPolicyEvaluator_EdgeCase_ThresholdEqualsRatio tests the boundary case where threshold exactly equals decision ratio
// Validates: Requirements 12.1
func TestPolicyEvaluator_EdgeCase_ThresholdEqualsRatio(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		decisionRatio float64
		threshold     float64
		expected      bool
		description   string
	}{
		{
			name:          "ratio equals threshold at 1.0",
			decisionRatio: 1.0,
			threshold:     1.0,
			expected:      true,
			description:   "When ratio == threshold, move should be executed (>= comparison)",
		},
		{
			name:          "ratio equals threshold at 1.5",
			decisionRatio: 1.5,
			threshold:     1.5,
			expected:      true,
			description:   "When ratio == threshold at 1.5, move should be executed",
		},
		{
			name:          "ratio equals threshold at 0.5",
			decisionRatio: 0.5,
			threshold:     0.5,
			expected:      true,
			description:   "When ratio == threshold at 0.5, move should be executed",
		},
		{
			name:          "ratio equals threshold at 2.0",
			decisionRatio: 2.0,
			threshold:     2.0,
			expected:      true,
			description:   "When ratio == threshold at 2.0, move should be executed",
		},
		{
			name:          "ratio equals threshold at 0.001",
			decisionRatio: 0.001,
			threshold:     0.001,
			expected:      true,
			description:   "When ratio == threshold at very small value, move should be executed",
		},
		{
			name:          "ratio equals threshold at 100.0",
			decisionRatio: 100.0,
			threshold:     100.0,
			expected:      true,
			description:   "When ratio == threshold at very large value, move should be executed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(5)
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenCostJustifiesDisruption,
				candidate,
				tt.decisionRatio,
				tt.threshold,
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v. %s", result, tt.expected, tt.description)
			}
		})
	}
}

// TestPolicyEvaluator_EdgeCase_InfiniteRatio tests handling of infinite decision ratio
// Validates: Requirements 12.2
func TestPolicyEvaluator_EdgeCase_InfiniteRatio(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name        string
		threshold   float64
		expected    bool
		description string
	}{
		{
			name:        "infinite ratio with threshold 1.0",
			threshold:   1.0,
			expected:    true,
			description: "Infinite ratio should be treated as greater than any finite threshold",
		},
		{
			name:        "infinite ratio with threshold 2.0",
			threshold:   2.0,
			expected:    true,
			description: "Infinite ratio should be treated as greater than threshold 2.0",
		},
		{
			name:        "infinite ratio with threshold 0.5",
			threshold:   0.5,
			expected:    true,
			description: "Infinite ratio should be treated as greater than threshold 0.5",
		},
		{
			name:        "infinite ratio with very high threshold 100.0",
			threshold:   100.0,
			expected:    true,
			description: "Infinite ratio should be treated as greater than even very high thresholds",
		},
		{
			name:        "infinite ratio with very low threshold 0.001",
			threshold:   0.001,
			expected:    true,
			description: "Infinite ratio should be treated as greater than even very low thresholds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(5)
			// Use math.Inf(1) for positive infinity
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenCostJustifiesDisruption,
				candidate,
				math.Inf(1), // Infinite decision ratio
				tt.threshold,
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v. %s", result, tt.expected, tt.description)
			}
		})
	}
}

// TestPolicyEvaluator_EdgeCase_NaNRatio tests handling of NaN decision ratio
// Validates: Requirements 12.3
func TestPolicyEvaluator_EdgeCase_NaNRatio(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name        string
		threshold   float64
		expected    bool
		description string
	}{
		{
			name:        "NaN ratio with threshold 1.0",
			threshold:   1.0,
			expected:    false,
			description: "NaN ratio should be treated as less than any threshold (NaN comparisons return false)",
		},
		{
			name:        "NaN ratio with threshold 2.0",
			threshold:   2.0,
			expected:    false,
			description: "NaN ratio should be treated as less than threshold 2.0",
		},
		{
			name:        "NaN ratio with threshold 0.5",
			threshold:   0.5,
			expected:    false,
			description: "NaN ratio should be treated as less than threshold 0.5",
		},
		{
			name:        "NaN ratio with very high threshold 100.0",
			threshold:   100.0,
			expected:    false,
			description: "NaN ratio should be treated as less than even very high thresholds",
		},
		{
			name:        "NaN ratio with very low threshold 0.001",
			threshold:   0.001,
			expected:    false,
			description: "NaN ratio should be treated as less than even very low thresholds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(5)
			// Use math.NaN() for NaN value
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenCostJustifiesDisruption,
				candidate,
				math.NaN(), // NaN decision ratio
				tt.threshold,
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v. %s", result, tt.expected, tt.description)
			}
		})
	}
}

// TestPolicyEvaluator_EdgeCase_VerySmallThreshold tests handling of very small threshold values
// Validates: Requirements 1.2, 8.3
func TestPolicyEvaluator_EdgeCase_VerySmallThreshold(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		decisionRatio float64
		threshold     float64
		expected      bool
		description   string
	}{
		{
			name:          "very small threshold 0.001 with ratio 0.002",
			decisionRatio: 0.002,
			threshold:     0.001,
			expected:      true,
			description:   "Very small threshold should allow moves with slightly higher ratios",
		},
		{
			name:          "very small threshold 0.001 with ratio 0.001",
			decisionRatio: 0.001,
			threshold:     0.001,
			expected:      true,
			description:   "Very small threshold should allow moves with equal ratio (boundary)",
		},
		{
			name:          "very small threshold 0.001 with ratio 0.0005",
			decisionRatio: 0.0005,
			threshold:     0.001,
			expected:      false,
			description:   "Very small threshold should reject moves with lower ratios",
		},
		{
			name:          "very small threshold 0.001 with ratio 1.0",
			decisionRatio: 1.0,
			threshold:     0.001,
			expected:      true,
			description:   "Very small threshold should allow nearly all normal moves",
		},
		{
			name:          "very small threshold 0.001 with ratio 0.5",
			decisionRatio: 0.5,
			threshold:     0.001,
			expected:      true,
			description:   "Very small threshold should allow moves with moderate ratios",
		},
		{
			name:          "very small threshold 0.001 with ratio 2.0",
			decisionRatio: 2.0,
			threshold:     0.001,
			expected:      true,
			description:   "Very small threshold should allow moves with high ratios",
		},
		{
			name:          "very small threshold 0.001 with ratio 0.0",
			decisionRatio: 0.0,
			threshold:     0.001,
			expected:      false,
			description:   "Very small threshold should reject zero ratio",
		},
		{
			name:          "threshold 0.0001 with ratio 0.0002",
			decisionRatio: 0.0002,
			threshold:     0.0001,
			expected:      true,
			description:   "Even smaller threshold should work correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(5)
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenCostJustifiesDisruption,
				candidate,
				tt.decisionRatio,
				tt.threshold,
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v. %s", result, tt.expected, tt.description)
			}
		})
	}
}

// TestPolicyEvaluator_EdgeCase_VeryLargeThreshold tests handling of very large threshold values
// Validates: Requirements 1.2, 8.3
func TestPolicyEvaluator_EdgeCase_VeryLargeThreshold(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		decisionRatio float64
		threshold     float64
		expected      bool
		description   string
	}{
		{
			name:          "very large threshold 100.0 with ratio 101.0",
			decisionRatio: 101.0,
			threshold:     100.0,
			expected:      true,
			description:   "Very large threshold should allow moves with higher ratios",
		},
		{
			name:          "very large threshold 100.0 with ratio 100.0",
			decisionRatio: 100.0,
			threshold:     100.0,
			expected:      true,
			description:   "Very large threshold should allow moves with equal ratio (boundary)",
		},
		{
			name:          "very large threshold 100.0 with ratio 99.0",
			decisionRatio: 99.0,
			threshold:     100.0,
			expected:      false,
			description:   "Very large threshold should reject moves with lower ratios",
		},
		{
			name:          "very large threshold 100.0 with ratio 1.0",
			decisionRatio: 1.0,
			threshold:     100.0,
			expected:      false,
			description:   "Very large threshold should block nearly all normal moves",
		},
		{
			name:          "very large threshold 100.0 with ratio 2.0",
			decisionRatio: 2.0,
			threshold:     100.0,
			expected:      false,
			description:   "Very large threshold should block moves with moderate ratios",
		},
		{
			name:          "very large threshold 100.0 with ratio 50.0",
			decisionRatio: 50.0,
			threshold:     100.0,
			expected:      false,
			description:   "Very large threshold should block moves with high but insufficient ratios",
		},
		{
			name:          "very large threshold 100.0 with ratio 0.5",
			decisionRatio: 0.5,
			threshold:     100.0,
			expected:      false,
			description:   "Very large threshold should block moves with low ratios",
		},
		{
			name:          "threshold 1000.0 with ratio 1001.0",
			decisionRatio: 1001.0,
			threshold:     1000.0,
			expected:      true,
			description:   "Even larger threshold should work correctly",
		},
		{
			name:          "threshold 1000.0 with ratio 500.0",
			decisionRatio: 500.0,
			threshold:     1000.0,
			expected:      false,
			description:   "Even larger threshold should block insufficient ratios",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(5)
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenCostJustifiesDisruption,
				candidate,
				tt.decisionRatio,
				tt.threshold,
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v. %s", result, tt.expected, tt.description)
			}
		})
	}
}

// TestPolicyEvaluator_EdgeCase_FloatingPointPrecision tests floating-point comparison edge cases
// Validates: Requirements 12.4
func TestPolicyEvaluator_EdgeCase_FloatingPointPrecision(t *testing.T) {
	evaluator := &PolicyEvaluator{}

	tests := []struct {
		name          string
		decisionRatio float64
		threshold     float64
		expected      bool
		description   string
	}{
		{
			name:          "ratio slightly below threshold due to floating-point precision",
			decisionRatio: 0.9999999999,
			threshold:     1.0,
			expected:      false,
			description:   "Standard >= comparison should be used (no epsilon), ratio < threshold",
		},
		{
			name:          "ratio slightly above threshold due to floating-point precision",
			decisionRatio: 1.0000000001,
			threshold:     1.0,
			expected:      true,
			description:   "Standard >= comparison should be used (no epsilon), ratio > threshold",
		},
		{
			name:          "ratio very close to threshold from below",
			decisionRatio: 1.0 - 1e-10,
			threshold:     1.0,
			expected:      false,
			description:   "No epsilon-based comparison, ratio < threshold should not consolidate",
		},
		{
			name:          "ratio very close to threshold from above",
			decisionRatio: 1.0 + 1e-10,
			threshold:     1.0,
			expected:      true,
			description:   "No epsilon-based comparison, ratio > threshold should consolidate",
		},
		{
			name:          "floating-point arithmetic result below threshold",
			decisionRatio: 0.1 + 0.2 + 0.6, // May not equal exactly 0.9 due to floating-point
			threshold:     1.0,
			expected:      false,
			description:   "Floating-point arithmetic results should use standard comparison",
		},
		{
			name:          "floating-point arithmetic result at threshold",
			decisionRatio: 0.3 + 0.3 + 0.4, // May not equal exactly 1.0 due to floating-point
			threshold:     1.0,
			expected:      true,
			description:   "Floating-point arithmetic results should use standard comparison",
		},
		{
			name:          "ratio 1.5 - epsilon with threshold 1.5",
			decisionRatio: 1.5 - 1e-15,
			threshold:     1.5,
			expected:      false,
			description:   "Very small difference below threshold should not consolidate",
		},
		{
			name:          "ratio 1.5 + epsilon with threshold 1.5",
			decisionRatio: 1.5 + 1e-15,
			threshold:     1.5,
			expected:      true,
			description:   "Very small difference above threshold should consolidate",
		},
		{
			name:          "ratio 2.0 - epsilon with threshold 2.0",
			decisionRatio: 2.0 - 1e-14,
			threshold:     2.0,
			expected:      false,
			description:   "Standard comparison at different threshold values",
		},
		{
			name:          "ratio 0.5 - epsilon with threshold 0.5",
			decisionRatio: 0.5 - 1e-15,
			threshold:     0.5,
			expected:      false,
			description:   "Standard comparison with aggressive threshold",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := createCandidateWithPods(5)
			result := evaluator.ShouldConsolidate(
				v1.ConsolidateWhenCostJustifiesDisruption,
				candidate,
				tt.decisionRatio,
				tt.threshold,
			)
			if result != tt.expected {
				t.Errorf("ShouldConsolidate() = %v, expected %v. %s (ratio=%v, threshold=%v)",
					result, tt.expected, tt.description, tt.decisionRatio, tt.threshold)
			}
		})
	}
}
