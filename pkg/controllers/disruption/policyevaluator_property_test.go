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

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Feature: configurable-decision-ratio-threshold, Property 3: Threshold Comparison in Policy Evaluation
// Validates: Requirements 2.1, 2.2
//
// This property test verifies that for any decision ratio and any positive threshold,
// when ConsolidateWhen is set to WhenCostJustifiesDisruption, the PolicyEvaluator
// executes consolidation if and only if the decision ratio is greater than or equal
// to the configured threshold.
func TestProperty_ThresholdComparisonInPolicyEvaluation(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("consolidation occurs if and only if ratio >= threshold for WhenCostJustifiesDisruption",
		prop.ForAll(
			func(threshold float64, decisionRatio float64) bool {
				evaluator := &PolicyEvaluator{}

				// Create a test candidate with some pods
				candidate := &Candidate{
					reschedulablePods: []*corev1.Pod{
						{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "test-pod",
								Namespace: "default",
							},
						},
					},
				}

				// Test the WhenCostJustifiesDisruption policy
				result := evaluator.ShouldConsolidate(
					v1.ConsolidateWhenCostJustifiesDisruption,
					candidate,
					decisionRatio,
					threshold,
				)

				// Expected result: consolidate if and only if ratio >= threshold
				expected := decisionRatio >= threshold

				// Verify the result matches the expected behavior
				if result != expected {
					return false
				}

				// Additional verification: test boundary cases
				// When ratio equals threshold, should consolidate
				if decisionRatio == threshold && !result {
					return false
				}

				// When ratio is slightly above threshold, should consolidate
				if decisionRatio > threshold && !result {
					return false
				}

				// When ratio is slightly below threshold, should not consolidate
				if decisionRatio < threshold && result {
					return false
				}

				return true
			},
			genPositiveThreshold(),
			genDecisionRatio(),
		))

	properties.TestingRun(t)
}

// genPositiveThreshold generates random positive threshold values
func genPositiveThreshold() gopter.Gen {
	return gen.Float64Range(0.001, 1000.0)
}

// genDecisionRatio generates random decision ratio values
func genDecisionRatio() gopter.Gen {
	return gen.OneGenOf(
		// Normal range of decision ratios
		gen.Float64Range(0.0, 10.0),
		// Include some edge cases
		gen.Const(0.0),
		gen.Const(1.0),
		gen.Const(math.Inf(1)), // Positive infinity
	)
}

// Feature: configurable-decision-ratio-threshold, Property 4: Threshold Ignored for Other Policies
// Validates: Requirements 2.3
//
// This property test verifies that for any threshold value, when ConsolidateWhen is set
// to WhenEmpty or WhenEmptyOrUnderutilized, consolidation decisions are independent of
// the threshold value. The threshold should only affect WhenCostJustifiesDisruption policy.
func TestProperty_ThresholdIgnoredForOtherPolicies(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("WhenEmpty and WhenEmptyOrUnderutilized policies ignore threshold",
		prop.ForAll(
			func(threshold1 float64, threshold2 float64, decisionRatio float64, hasPods bool) bool {
				evaluator := &PolicyEvaluator{}

				// Create a test candidate with or without pods based on hasPods
				var pods []*corev1.Pod
				if hasPods {
					pods = []*corev1.Pod{
						{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "test-pod",
								Namespace: "default",
							},
						},
					}
				}

				candidate := &Candidate{
					reschedulablePods: pods,
				}

				// Test WhenEmpty policy with two different thresholds
				resultEmpty1 := evaluator.ShouldConsolidate(
					v1.ConsolidateWhenEmpty,
					candidate,
					decisionRatio,
					threshold1,
				)

				resultEmpty2 := evaluator.ShouldConsolidate(
					v1.ConsolidateWhenEmpty,
					candidate,
					decisionRatio,
					threshold2,
				)

				// WhenEmpty should produce the same result regardless of threshold
				if resultEmpty1 != resultEmpty2 {
					return false
				}

				// WhenEmpty should only depend on whether there are pods
				expectedEmpty := len(pods) == 0
				if resultEmpty1 != expectedEmpty {
					return false
				}

				// Test WhenEmptyOrUnderutilized policy with two different thresholds
				resultUnderutilized1 := evaluator.ShouldConsolidate(
					v1.ConsolidateWhenEmptyOrUnderutilized,
					candidate,
					decisionRatio,
					threshold1,
				)

				resultUnderutilized2 := evaluator.ShouldConsolidate(
					v1.ConsolidateWhenEmptyOrUnderutilized,
					candidate,
					decisionRatio,
					threshold2,
				)

				// WhenEmptyOrUnderutilized should produce the same result regardless of threshold
				if resultUnderutilized1 != resultUnderutilized2 {
					return false
				}

				// WhenEmptyOrUnderutilized should always return true
				if !resultUnderutilized1 || !resultUnderutilized2 {
					return false
				}

				return true
			},
			genPositiveThreshold(),
			genPositiveThreshold(),
			genDecisionRatio(),
			gen.Bool(),
		))

	properties.TestingRun(t)
}
