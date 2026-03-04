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
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Feature: consolidation-decision-ratio-control, Property 6: Policy Filtering at Break-Even Threshold
// Validates: Requirements 2.1
//
// This property test verifies that when ConsolidateWhen is set to WhenCostJustifiesDisruption,
// consolidation moves are executed if and only if the decision ratio is greater than or equal to 1.0.
func TestProperty_PolicyFilteringAtBreakEvenThreshold(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("moves with ratio >= 1.0 should be consolidated when policy is WhenCostJustifiesDisruption",
		prop.ForAll(
			func(decisionRatio float64, podCount int) bool {
				// Create a policy evaluator
				evaluator := &PolicyEvaluator{}

				// Create a mock candidate with the given pod count
				candidate := &Candidate{
					reschedulablePods: make([]*corev1.Pod, podCount),
				}

				// Test the policy evaluation
				result := evaluator.ShouldConsolidate(
					v1.ConsolidateWhenCostJustifiesDisruption,
					candidate,
					decisionRatio,
					1.0, // threshold parameter (using default 1.0)
				)

				// The property: should consolidate if and only if ratio >= 1.0
				expectedResult := decisionRatio >= 1.0

				return result == expectedResult
			},
			// Generate decision ratios from 0.0 to 10.0
			gen.Float64Range(0.0, 10.0),
			// Generate pod counts from 0 to 100
			gen.IntRange(0, 100),
		))

	properties.Property("WhenEmpty policy should only consolidate nodes with zero pods",
		prop.ForAll(
			func(decisionRatio float64, podCount int) bool {
				evaluator := &PolicyEvaluator{}

				candidate := &Candidate{
					reschedulablePods: make([]*corev1.Pod, podCount),
				}

				result := evaluator.ShouldConsolidate(
					v1.ConsolidateWhenEmpty,
					candidate,
					decisionRatio,
					1.0, // threshold parameter (ignored for WhenEmpty policy)
				)

				// The property: should consolidate if and only if pod count is 0
				expectedResult := podCount == 0

				return result == expectedResult
			},
			gen.Float64Range(0.0, 10.0),
			gen.IntRange(0, 100),
		))

	properties.Property("WhenEmptyOrUnderutilized policy should always consolidate regardless of ratio",
		prop.ForAll(
			func(decisionRatio float64, podCount int) bool {
				evaluator := &PolicyEvaluator{}

				candidate := &Candidate{
					reschedulablePods: make([]*corev1.Pod, podCount),
				}

				result := evaluator.ShouldConsolidate(
					v1.ConsolidateWhenEmptyOrUnderutilized,
					candidate,
					decisionRatio,
					1.0, // threshold parameter (ignored for WhenEmptyOrUnderutilized policy)
				)

				// The property: should always return true
				return result == true
			},
			gen.Float64Range(0.0, 10.0),
			gen.IntRange(0, 100),
		))

	properties.TestingRun(t)
}
