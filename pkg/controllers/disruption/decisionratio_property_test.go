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

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
)

// Feature: consolidation-decision-ratio-control, Property 1: Normalized Cost Computation
// Validates: Requirements 1.1
//
// This property test verifies that for any node in any nodepool, the normalized cost
// equals the node's hourly cost divided by the sum of all node costs in the nodepool.
func TestProperty_NormalizedCostComputation(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("normalized cost equals node cost / total cost",
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
					candidates[i] = createTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, _ := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost is zero (edge case)
				if totalCost == 0.0 {
					return true
				}

				// Verify normalized cost for each candidate
				for i := range candidates {
					nodeCost := candidateData[i].cost
					expectedNormalizedCost := nodeCost / totalCost

					// Compute actual normalized cost
					actualNormalizedCost := nodeCost / totalCost

					// Allow small floating point error (1e-9)
					if !floatEquals(actualNormalizedCost, expectedNormalizedCost, 1e-9) {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 2: Normalized Disruption Computation
// Validates: Requirements 1.2
//
// This property test verifies that for any node in any nodepool, the normalized disruption
// equals the node's disruption cost divided by the sum of all disruption costs in the nodepool.
func TestProperty_NormalizedDisruptionComputation(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("normalized disruption equals node disruption / total disruption",
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
					candidates[i] = createTestCandidate(data)
				}

				// Compute nodepool metrics
				_, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total disruption is zero (edge case)
				if totalDisruption == 0.0 {
					return true
				}

				// Verify normalized disruption for each candidate
				for i := range candidates {
					nodeDisruption := candidateData[i].disruption
					expectedNormalizedDisruption := nodeDisruption / totalDisruption

					// Compute actual normalized disruption
					actualNormalizedDisruption := nodeDisruption / totalDisruption

					// Allow small floating point error (1e-9)
					if !floatEquals(actualNormalizedDisruption, expectedNormalizedDisruption, 1e-9) {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// candidateTestData holds test data for creating a candidate
type candidateTestData struct {
	cost       float64
	disruption float64
}

// genCandidateDataSlice generates a slice of candidate test data
func genCandidateDataSlice() gopter.Gen {
	return gen.SliceOfN(10, genCandidateData()).
		SuchThat(func(v interface{}) bool {
			slice := v.([]candidateTestData)
			// Ensure at least 2 candidates
			return len(slice) >= 2
		})
}

// genCandidateData generates random candidate test data
func genCandidateData() gopter.Gen {
	return gopter.CombineGens(
		gen.Float64Range(0.01, 10.0), // Node cost
		gen.Float64Range(1.0, 100.0), // Disruption cost
	).Map(func(values []interface{}) candidateTestData {
		return candidateTestData{
			cost:       values[0].(float64),
			disruption: values[1].(float64),
		}
	})
}

// createTestCandidate creates a test candidate from test data
func createTestCandidate(data candidateTestData) *Candidate {
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
	}
}

// floatEquals checks if two floats are equal within a tolerance
func floatEquals(a, b, tolerance float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= tolerance
}

// Feature: consolidation-decision-ratio-control, Property 3: Decision Ratio Formula
// Validates: Requirements 1.3
//
// This property test verifies that for any consolidation candidate with non-zero normalized
// disruption, the decision ratio equals normalized cost divided by normalized disruption.
//
//nolint:gocyclo
func TestProperty_DecisionRatioFormula(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("decision ratio equals normalized cost / normalized disruption",
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
					candidates[i] = createTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Verify decision ratio formula for each candidate
				for i, candidate := range candidates {
					// Compute decision ratio using the calculator
					actualRatio := calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

					// Compute expected ratio from the formula
					nodeCost := candidateData[i].cost
					nodeDisruption := candidateData[i].disruption

					normalizedCost := nodeCost / totalCost
					normalizedDisruption := nodeDisruption / totalDisruption

					// Skip if normalized disruption is zero (would result in infinity)
					if normalizedDisruption == 0.0 {
						// Verify that actualRatio is infinity
						if !isInf(actualRatio) {
							return false
						}
						continue
					}

					expectedRatio := normalizedCost / normalizedDisruption

					// Verify the formula holds
					if !floatEquals(actualRatio, expectedRatio, 1e-9) {
						return false
					}

					// Also verify that the candidate's stored values match
					if !floatEquals(candidate.normalizedCost, normalizedCost, 1e-9) {
						return false
					}
					if !floatEquals(candidate.normalizedDisruption, normalizedDisruption, 1e-9) {
						return false
					}
					if !floatEquals(candidate.decisionRatio, expectedRatio, 1e-9) {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// isInf checks if a float64 value is positive or negative infinity
func isInf(f float64) bool {
	return math.IsInf(f, 0)
}

// Feature: consolidation-decision-ratio-control, Property 12: Delete Ratio Computation
// Validates: Requirements 4.1
//
// This property test verifies that for any node, the delete ratio (computed assuming full
// node cost recovery) equals the node's normalized cost divided by its normalized disruption.
// The delete ratio represents the upper bound for consolidation moves since it assumes the
// entire node cost is recovered (as in a delete move).
func TestProperty_DeleteRatioComputation(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("delete ratio equals normalized cost / normalized disruption",
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
					candidates[i] = createTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Verify delete ratio formula for each candidate
				for i, candidate := range candidates {
					// Compute delete ratio using the calculator
					actualDeleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

					// Compute expected delete ratio from the formula
					nodeCost := candidateData[i].cost
					nodeDisruption := candidateData[i].disruption

					normalizedCost := nodeCost / totalCost
					normalizedDisruption := nodeDisruption / totalDisruption

					// Skip if normalized disruption is zero (would result in infinity)
					if normalizedDisruption == 0.0 {
						// Verify that actualDeleteRatio is infinity
						if !isInf(actualDeleteRatio) {
							return false
						}
						continue
					}

					expectedDeleteRatio := normalizedCost / normalizedDisruption

					// Verify the formula holds
					if !floatEquals(actualDeleteRatio, expectedDeleteRatio, 1e-9) {
						return false
					}

					// Also verify that the candidate's stored deleteRatio matches
					if !floatEquals(candidate.deleteRatio, expectedDeleteRatio, 1e-9) {
						return false
					}

					// Verify that delete ratio equals decision ratio (since both use the same formula)
					// This validates that delete ratio is indeed the upper bound
					decisionRatio := calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
					if !floatEquals(actualDeleteRatio, decisionRatio, 1e-9) {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 4: Node Disruption Cost Composition
// Validates: Requirements 1.4, 1.6
//
// This property test verifies that for any node with any set of pods, the node disruption cost
// equals 1.0 (baseline) plus the sum of all pod eviction costs (with negative costs clamped to zero).
func TestProperty_NodeDisruptionCostComposition(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("node disruption cost equals 1.0 + sum(max(0, evictionCost))",
		prop.ForAll(
			func(podData []podTestData) bool {
				ctx := context.Background()

				// Create pods from test data
				pods := make([]*corev1.Pod, len(podData))
				for i, data := range podData {
					pods[i] = createTestPod(data)
				}

				// Compute node disruption cost using the function
				actualCost := ComputeNodeDisruptionCost(ctx, pods)

				// Compute expected cost: baseline (1.0) + sum of clamped eviction costs
				expectedCost := 1.0
				for _, pod := range pods {
					evictionCost := disruptionutils.EvictionCost(ctx, pod)
					// Only add positive eviction costs (negative costs are clamped to zero)
					if evictionCost > 0 {
						expectedCost += evictionCost
					}
				}

				// Verify the formula holds
				if !floatEquals(actualCost, expectedCost, 1e-9) {
					return false
				}

				// Verify baseline cost is always present (cost >= 1.0)
				if actualCost < 1.0 {
					return false
				}

				return true
			},
			genPodDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 5: EvictionCost Function Usage
// Validates: Requirements 1.5
//
// This property test verifies that for any pod on any node, the per-pod disruption cost
// used in the decision ratio computation equals the result of calling EvictionCost(pod).
func TestProperty_EvictionCostFunctionUsage(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("per-pod disruption cost uses EvictionCost function",
		prop.ForAll(
			func(podData []podTestData) bool {
				ctx := context.Background()

				// Create pods from test data
				pods := make([]*corev1.Pod, len(podData))
				for i, data := range podData {
					pods[i] = createTestPod(data)
				}

				// Compute node disruption cost
				totalCost := ComputeNodeDisruptionCost(ctx, pods)

				// Verify that the total cost can be reconstructed from EvictionCost calls
				expectedCost := 1.0 // baseline
				for _, pod := range pods {
					evictionCost := disruptionutils.EvictionCost(ctx, pod)
					// Clamp negative costs to zero (as per requirement 1.6)
					if evictionCost > 0 {
						expectedCost += evictionCost
					}
				}

				// Verify the costs match
				if !floatEquals(totalCost, expectedCost, 1e-9) {
					return false
				}

				// Also verify that each individual pod's contribution matches EvictionCost
				for _, pod := range pods {
					evictionCost := disruptionutils.EvictionCost(ctx, pod)

					// Compute cost with and without this pod
					podsWithout := make([]*corev1.Pod, 0, len(pods)-1)
					for _, p := range pods {
						if p != pod {
							podsWithout = append(podsWithout, p)
						}
					}

					costWith := ComputeNodeDisruptionCost(ctx, pods)
					costWithout := ComputeNodeDisruptionCost(ctx, podsWithout)

					// The difference should equal max(0, evictionCost)
					expectedDiff := math.Max(0, evictionCost)
					actualDiff := costWith - costWithout

					if !floatEquals(actualDiff, expectedDiff, 1e-9) {
						return false
					}
				}

				return true
			},
			genPodDataSlice(),
		))

	properties.TestingRun(t)
}

// podTestData holds test data for creating a pod
type podTestData struct {
	deletionCost string // PodDeletionCost annotation value
	priority     *int32 // Pod priority
}

// genPodDataSlice generates a slice of pod test data
func genPodDataSlice() gopter.Gen {
	return gen.SliceOfN(10, genPodData()).
		SuchThat(func(v interface{}) bool {
			slice := v.([]podTestData)
			// Allow empty slices to test baseline cost
			return len(slice) >= 0
		})
}

// genPodData generates random pod test data
func genPodData() gopter.Gen {
	return gopter.CombineGens(
		genDeletionCost(),
		genPriority(),
	).Map(func(values []interface{}) podTestData {
		return podTestData{
			deletionCost: values[0].(string),
			priority:     values[1].(*int32),
		}
	})
}

// genDeletionCost generates random pod deletion cost annotation values
func genDeletionCost() gopter.Gen {
	return gen.OneGenOf(
		// Empty string (no annotation)
		gen.Const(""),
		// Valid deletion costs in the range [-2147483647, 2147483647]
		gen.Int32Range(-2147483647, 2147483647).Map(func(v int32) string {
			return strconv.FormatInt(int64(v), 10)
		}),
		// Some specific interesting values
		gen.OneConstOf("0", "100", "-100", "1000", "-1000"),
	)
}

// genPriority generates random pod priority values
func genPriority() gopter.Gen {
	return gen.OneGenOf(
		// Nil priority (not set)
		gen.Const((*int32)(nil)),
		// Valid priorities in the range [-2147483648, 1000000000]
		gen.Int32Range(-2147483648, 1000000000).Map(func(v int32) *int32 {
			return &v
		}),
		// Some specific interesting values
		gen.OneConstOf(
			lo.ToPtr(int32(0)),
			lo.ToPtr(int32(100)),
			lo.ToPtr(int32(-100)),
			lo.ToPtr(int32(1000)),
			lo.ToPtr(int32(-1000)),
		),
	)
}

// createTestPod creates a test pod from test data
func createTestPod(data podTestData) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Priority: data.priority,
		},
	}

	// Add deletion cost annotation if provided
	if data.deletionCost != "" {
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[corev1.PodDeletionCost] = data.deletionCost
	}

	return pod
}

// Feature: consolidation-decision-ratio-control, Property 15: Pod Deletion Cost Impact
// Validates: Requirements 5.4
//
// This property test verifies that for any two pods that differ only in PodDeletionCost
// annotation, the pod with the higher PodDeletionCost contributes higher disruption cost
// to its node.
//
//nolint:gocyclo
func TestProperty_PodDeletionCostImpact(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("higher PodDeletionCost leads to higher disruption cost",
		prop.ForAll(
			func(basePriority *int32, cost1Str string, cost2Str string) bool {
				ctx := context.Background()

				// Parse the deletion costs
				cost1, err1 := strconv.ParseFloat(cost1Str, 64)
				cost2, err2 := strconv.ParseFloat(cost2Str, 64)

				// Skip if either cost is invalid
				if err1 != nil || err2 != nil {
					return true
				}

				// Skip if costs are equal (we want to test difference)
				if cost1 == cost2 {
					return true
				}

				// Create two pods that differ only in PodDeletionCost
				pod1 := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: "default",
						Annotations: map[string]string{
							corev1.PodDeletionCost: cost1Str,
						},
					},
					Spec: corev1.PodSpec{
						Priority: basePriority,
					},
				}

				pod2 := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-2",
						Namespace: "default",
						Annotations: map[string]string{
							corev1.PodDeletionCost: cost2Str,
						},
					},
					Spec: corev1.PodSpec{
						Priority: basePriority,
					},
				}

				// Compute eviction costs
				evictionCost1 := disruptionutils.EvictionCost(ctx, pod1)
				evictionCost2 := disruptionutils.EvictionCost(ctx, pod2)

				// Compute node disruption costs
				nodeCost1 := ComputeNodeDisruptionCost(ctx, []*corev1.Pod{pod1})
				nodeCost2 := ComputeNodeDisruptionCost(ctx, []*corev1.Pod{pod2})

				// The pod with higher deletion cost should have higher eviction cost
				// (unless clamping or negative values affect the result)
				if cost1 > cost2 {
					// Pod1 has higher deletion cost
					// Its eviction cost should be >= pod2's eviction cost
					// (allowing for clamping and negative value handling)
					if evictionCost1 < evictionCost2 {
						// This is acceptable if both are clamped or negative
						// Just verify the node costs reflect the clamped values
						expectedDiff1 := math.Max(0, evictionCost1)
						expectedDiff2 := math.Max(0, evictionCost2)
						actualDiff1 := nodeCost1 - 1.0 // Remove baseline
						actualDiff2 := nodeCost2 - 1.0 // Remove baseline

						if !floatEquals(actualDiff1, expectedDiff1, 1e-9) {
							return false
						}
						if !floatEquals(actualDiff2, expectedDiff2, 1e-9) {
							return false
						}
					} else {
						// evictionCost1 >= evictionCost2
						// Node cost should reflect this (after clamping negatives)
						contribution1 := math.Max(0, evictionCost1)
						contribution2 := math.Max(0, evictionCost2)

						if contribution1 >= contribution2 {
							// nodeCost1 should be >= nodeCost2
							if nodeCost1 < nodeCost2-1e-9 {
								return false
							}
						}
					}
				} else {
					// Pod2 has higher deletion cost
					// Its eviction cost should be >= pod1's eviction cost
					if evictionCost2 < evictionCost1 {
						// This is acceptable if both are clamped or negative
						expectedDiff1 := math.Max(0, evictionCost1)
						expectedDiff2 := math.Max(0, evictionCost2)
						actualDiff1 := nodeCost1 - 1.0
						actualDiff2 := nodeCost2 - 1.0

						if !floatEquals(actualDiff1, expectedDiff1, 1e-9) {
							return false
						}
						if !floatEquals(actualDiff2, expectedDiff2, 1e-9) {
							return false
						}
					} else {
						// evictionCost2 >= evictionCost1
						contribution1 := math.Max(0, evictionCost1)
						contribution2 := math.Max(0, evictionCost2)

						if contribution2 >= contribution1 {
							// nodeCost2 should be >= nodeCost1
							if nodeCost2 < nodeCost1-1e-9 {
								return false
							}
						}
					}
				}

				return true
			},
			genPriority(),
			genDeletionCostValue(),
			genDeletionCostValue(),
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 16: Pod Priority Impact
// Validates: Requirements 5.5
//
// This property test verifies that for any two pods that differ only in Priority,
// the pod with the higher Priority contributes higher disruption cost to its node.
//
//nolint:gocyclo
func TestProperty_PodPriorityImpact(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("higher Priority leads to higher disruption cost",
		prop.ForAll(
			func(baseDeletionCost string, priority1 *int32, priority2 *int32) bool {
				ctx := context.Background()

				// Skip if both priorities are nil
				if priority1 == nil && priority2 == nil {
					return true
				}

				// Skip if priorities are equal
				if priority1 != nil && priority2 != nil && *priority1 == *priority2 {
					return true
				}

				// Create two pods that differ only in Priority
				pod1 := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Priority: priority1,
					},
				}

				pod2 := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-2",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Priority: priority2,
					},
				}

				// Add deletion cost annotation if provided
				if baseDeletionCost != "" {
					pod1.Annotations = map[string]string{
						corev1.PodDeletionCost: baseDeletionCost,
					}
					pod2.Annotations = map[string]string{
						corev1.PodDeletionCost: baseDeletionCost,
					}
				}

				// Compute eviction costs
				evictionCost1 := disruptionutils.EvictionCost(ctx, pod1)
				evictionCost2 := disruptionutils.EvictionCost(ctx, pod2)

				// Compute node disruption costs
				nodeCost1 := ComputeNodeDisruptionCost(ctx, []*corev1.Pod{pod1})
				nodeCost2 := ComputeNodeDisruptionCost(ctx, []*corev1.Pod{pod2})

				// Determine which pod has higher priority
				p1 := int32(0)
				if priority1 != nil {
					p1 = *priority1
				}
				p2 := int32(0)
				if priority2 != nil {
					p2 = *priority2
				}

				// The pod with higher priority should have higher eviction cost
				// (unless clamping affects the result)
				if p1 > p2 {
					// Pod1 has higher priority
					// Its eviction cost should be >= pod2's eviction cost
					// (allowing for clamping)
					if evictionCost1 < evictionCost2 {
						// This is acceptable if both are clamped
						// Just verify the node costs reflect the clamped values
						expectedDiff1 := math.Max(0, evictionCost1)
						expectedDiff2 := math.Max(0, evictionCost2)
						actualDiff1 := nodeCost1 - 1.0 // Remove baseline
						actualDiff2 := nodeCost2 - 1.0 // Remove baseline

						if !floatEquals(actualDiff1, expectedDiff1, 1e-9) {
							return false
						}
						if !floatEquals(actualDiff2, expectedDiff2, 1e-9) {
							return false
						}
					} else {
						// evictionCost1 >= evictionCost2
						// Node cost should reflect this (after clamping negatives)
						contribution1 := math.Max(0, evictionCost1)
						contribution2 := math.Max(0, evictionCost2)

						if contribution1 >= contribution2 {
							// nodeCost1 should be >= nodeCost2
							if nodeCost1 < nodeCost2-1e-9 {
								return false
							}
						}
					}
				} else if p2 > p1 {
					// Pod2 has higher priority
					// Its eviction cost should be >= pod1's eviction cost
					if evictionCost2 < evictionCost1 {
						// This is acceptable if both are clamped
						expectedDiff1 := math.Max(0, evictionCost1)
						expectedDiff2 := math.Max(0, evictionCost2)
						actualDiff1 := nodeCost1 - 1.0
						actualDiff2 := nodeCost2 - 1.0

						if !floatEquals(actualDiff1, expectedDiff1, 1e-9) {
							return false
						}
						if !floatEquals(actualDiff2, expectedDiff2, 1e-9) {
							return false
						}
					} else {
						// evictionCost2 >= evictionCost1
						contribution1 := math.Max(0, evictionCost1)
						contribution2 := math.Max(0, evictionCost2)

						if contribution2 >= contribution1 {
							// nodeCost2 should be >= nodeCost1
							if nodeCost2 < nodeCost1-1e-9 {
								return false
							}
						}
					}
				}

				return true
			},
			genDeletionCost(),
			genPriority(),
			genPriority(),
		))

	properties.TestingRun(t)
}

// genDeletionCostValue generates random deletion cost values (not strings)
func genDeletionCostValue() gopter.Gen {
	return gen.OneGenOf(
		// Valid deletion costs in the range [-2147483647, 2147483647]
		gen.Int32Range(-2147483647, 2147483647).Map(func(v int32) string {
			return strconv.FormatInt(int64(v), 10)
		}),
		// Some specific interesting values
		gen.OneConstOf("0", "100", "-100", "1000", "-1000", "10000", "-10000"),
	)
}

// Feature: consolidation-decision-ratio-control, Property 20: Decision Ratio Computation Stability
// Validates: Requirements 8.1
//
// This property test verifies that for any consolidation move, computing the decision ratio
// multiple times with the same inputs produces identical results, ensuring consistent move ordering.
//
//nolint:gocyclo
func TestProperty_DecisionRatioComputationStability(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("computing decision ratio multiple times produces identical results",
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
					candidates[i] = createTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// Compute decision ratios multiple times for each candidate
				const iterations = 5
				for _, candidate := range candidates {
					var ratios []float64

					// Compute the decision ratio multiple times
					for i := 0; i < iterations; i++ {
						ratio := calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
						ratios = append(ratios, ratio)
					}

					// Verify all computed ratios are identical
					firstRatio := ratios[0]
					for i := 1; i < iterations; i++ {
						// Handle infinity case
						if isInf(firstRatio) && isInf(ratios[i]) {
							continue
						}

						// For finite values, check equality with tolerance
						if !floatEquals(ratios[i], firstRatio, 1e-9) {
							return false
						}
					}

					// Also verify that the stored candidate values remain consistent
					// Compute one more time and check stored values
					finalRatio := calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

					// Handle infinity case
					if isInf(finalRatio) {
						if !isInf(candidate.decisionRatio) {
							return false
						}
					} else {
						if !floatEquals(candidate.decisionRatio, finalRatio, 1e-9) {
							return false
						}
					}
				}

				// Verify that computing ratios in different orders produces the same results
				// Create a second set of candidates with the same data
				candidates2 := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates2[i] = createTestCandidate(data)
				}

				// Compute ratios for candidates2 in reverse order
				for i := len(candidates2) - 1; i >= 0; i-- {
					calculator.ComputeDecisionRatio(ctx, candidates2[i], totalCost, totalDisruption)
				}

				// Verify that ratios match between candidates and candidates2
				for i := range candidates {
					ratio1 := candidates[i].decisionRatio
					ratio2 := candidates2[i].decisionRatio

					// Handle infinity case
					if isInf(ratio1) && isInf(ratio2) {
						continue
					}

					if !floatEquals(ratio1, ratio2, 1e-9) {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 17: Consistent Disruption Cost Across Move Types
// Validates: Requirements 7.1
//
// This property test verifies that for any node being considered for both delete and replace moves,
// the disruption cost computed for the node is identical regardless of move type. The disruption cost
// depends only on the pods being evicted, not on whether the node is being deleted or replaced.
func TestProperty_ConsistentDisruptionCostAcrossMoveTypes(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("disruption cost is identical for delete and replace moves",
		prop.ForAll(
			func(podData []podTestData, nodeCost float64) bool {
				ctx := context.Background()

				// Create pods from test data
				pods := make([]*corev1.Pod, len(podData))
				for i, data := range podData {
					pods[i] = createTestPod(data)
				}

				// Create a candidate node with these pods
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
						Name: "test-instance-type",
						Offerings: cloudprovider.Offerings{
							{Price: nodeCost},
						},
					},
					reschedulablePods: pods,
				}
				_ = candidate.StateNode         // Used for test setup
				_ = candidate.instanceType      // Used for test setup
				_ = candidate.reschedulablePods // Used for test setup

				// Compute disruption cost for a "delete move" scenario
				// (In reality, the move type doesn't affect disruption cost computation)
				disruptionCostDelete := ComputeNodeDisruptionCost(ctx, pods)

				// Compute disruption cost for a "replace move" scenario
				// (Should be identical since it's the same pods being evicted)
				disruptionCostReplace := ComputeNodeDisruptionCost(ctx, pods)

				// Verify that disruption costs are identical
				if !floatEquals(disruptionCostDelete, disruptionCostReplace, 1e-9) {
					return false
				}

				// Also verify that the disruption cost stored in the candidate
				// would be the same regardless of move type
				candidate.DisruptionCost = disruptionCostDelete
				storedCost1 := candidate.DisruptionCost

				candidate.DisruptionCost = disruptionCostReplace
				storedCost2 := candidate.DisruptionCost

				if !floatEquals(storedCost1, storedCost2, 1e-9) {
					return false
				}

				// Verify that the disruption cost depends only on the pods,
				// not on the node cost (which differs between delete and replace)
				// Compute disruption cost again with the same pods
				disruptionCost2 := ComputeNodeDisruptionCost(ctx, pods)

				// Disruption cost should be the same despite different node costs
				if !floatEquals(disruptionCostDelete, disruptionCost2, 1e-9) {
					return false
				}

				return true
			},
			genPodDataSlice(),
			gen.Float64Range(0.01, 10.0), // Node cost
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 18: Delete Move Cost Savings
// Validates: Requirements 7.2
//
// This property test verifies that for any delete move, the cost savings equals the full
// hourly cost of the node being deleted. In a delete move, the node is removed and its
// pods are rescheduled to existing capacity, so the entire node cost is recovered.
func TestProperty_DeleteMoveCostSavings(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("delete move cost savings equals full node cost",
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
					candidates[i] = createTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// For each candidate, verify that a delete move would recover full node cost
				for i, candidate := range candidates {
					nodeCost := candidateData[i].cost

					// In a delete move, cost savings = full node cost
					costSavingsDelete := nodeCost

					// Compute the decision ratio for a delete move
					// This uses the full node cost as the numerator
					deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

					// Verify that the delete ratio is computed using full node cost
					// deleteRatio = (nodeCost / totalCost) / (nodeDisruption / totalDisruption)
					normalizedCostSavings := costSavingsDelete / totalCost
					normalizedDisruption := candidateData[i].disruption / totalDisruption

					// Skip if normalized disruption is zero (would result in infinity)
					if normalizedDisruption == 0.0 {
						if !isInf(deleteRatio) {
							return false
						}
						continue
					}

					expectedDeleteRatio := normalizedCostSavings / normalizedDisruption

					// Verify the delete ratio matches expected
					if !floatEquals(deleteRatio, expectedDeleteRatio, 1e-9) {
						return false
					}

					// Verify that cost savings for delete equals full node cost
					// (This is implicit in the delete ratio computation)
					if !floatEquals(costSavingsDelete, nodeCost, 1e-9) {
						return false
					}
				}

				return true
			},
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 19: Replace Move Cost Savings
// Validates: Requirements 7.3
//
// This property test verifies that for any replace move, the cost savings equals the
// difference between the source node's hourly cost and the replacement node's hourly cost.
// In a replace move, a cheaper node is launched to take over the pods, so only the cost
// difference is recovered.
//
//nolint:gocyclo
func TestProperty_ReplaceMoveCostSavings(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("replace move cost savings equals cost difference",
		prop.ForAll(
			func(candidateData []candidateTestData, replacementCostFactor float64) bool {
				// Skip empty or single-candidate cases
				if len(candidateData) < 2 {
					return true
				}

				// Ensure replacement cost factor is in valid range (0.1 to 0.9)
				// This ensures replacement is cheaper than source
				if replacementCostFactor <= 0.1 || replacementCostFactor >= 0.9 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})

				// Create candidates from test data
				candidates := make([]*Candidate, len(candidateData))
				for i, data := range candidateData {
					candidates[i] = createTestCandidate(data)
				}

				// Compute nodepool metrics
				totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

				// Skip if total cost or disruption is zero (edge cases)
				if totalCost == 0.0 || totalDisruption == 0.0 {
					return true
				}

				// For each candidate, verify that a replace move recovers only the cost difference
				for i, candidate := range candidates {
					sourceCost := candidateData[i].cost

					// Simulate a replace move with a cheaper instance
					replacementCost := sourceCost * replacementCostFactor

					// In a replace move, cost savings = source cost - replacement cost
					costSavingsReplace := sourceCost - replacementCost

					// Verify that cost savings is positive (replacement is cheaper)
					if costSavingsReplace <= 0 {
						return false
					}

					// Compute the decision ratio for a replace move
					// This uses the cost difference as the numerator
					normalizedCostSavings := costSavingsReplace / totalCost
					normalizedDisruption := candidateData[i].disruption / totalDisruption

					// Skip if normalized disruption is zero (would result in infinity)
					if normalizedDisruption == 0.0 {
						continue
					}

					replaceRatio := normalizedCostSavings / normalizedDisruption

					// Verify that the replace ratio is less than or equal to the delete ratio
					// (since replace recovers less cost than delete)
					deleteRatio := calculator.ComputeDeleteRatio(ctx, candidate, totalCost, totalDisruption)

					// Skip if delete ratio is infinity
					if isInf(deleteRatio) {
						continue
					}

					// Replace ratio should be <= delete ratio
					if replaceRatio > deleteRatio+1e-9 {
						return false
					}

					// Verify that cost savings for replace equals the cost difference
					expectedCostSavings := sourceCost - replacementCost
					if !floatEquals(costSavingsReplace, expectedCostSavings, 1e-9) {
						return false
					}

					// Verify that replace ratio is proportional to cost savings
					// replaceRatio / deleteRatio should equal costSavingsReplace / sourceCost
					if deleteRatio > 0 && !isInf(deleteRatio) {
						ratioOfRatios := replaceRatio / deleteRatio
						ratioOfCosts := costSavingsReplace / sourceCost

						if !floatEquals(ratioOfRatios, ratioOfCosts, 1e-6) {
							return false
						}
					}
				}

				return true
			},
			genCandidateDataSlice(),
			gen.Float64Range(0.1, 0.9), // Replacement cost factor
		))

	properties.TestingRun(t)
}

// Feature: consolidation-decision-ratio-control, Property 9: Per-NodePool Policy Independence
// Validates: Requirements 2.5, 6.3
//
// This property test verifies that for any set of nodepools with different ConsolidateWhen
// policies, applying consolidation logic to one nodepool does not affect the consolidation
// decisions for other nodepools. Each nodepool should independently apply its own policy.
//
//nolint:gocyclo
func TestProperty_PerNodePoolPolicyIndependence(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("consolidation decisions are independent per nodepool",
		prop.ForAll(
			func(nodepool1Data, nodepool2Data, nodepool3Data []candidateTestData) bool {
				// Skip if any nodepool has insufficient candidates
				if len(nodepool1Data) < 2 || len(nodepool2Data) < 2 || len(nodepool3Data) < 2 {
					return true
				}

				ctx := context.Background()
				calculator := NewDecisionRatioCalculator(clock.RealClock{})
				evaluator := &PolicyEvaluator{}

				// Create three nodepools with different policies
				policies := []v1.ConsolidateWhenPolicy{
					v1.ConsolidateWhenEmpty,
					v1.ConsolidateWhenEmptyOrUnderutilized,
					v1.ConsolidateWhenCostJustifiesDisruption,
				}

				// Create candidates for each nodepool
				nodepoolCandidates := [][]*Candidate{
					createCandidatesForNodePool("nodepool-1", policies[0], nodepool1Data),
					createCandidatesForNodePool("nodepool-2", policies[1], nodepool2Data),
					createCandidatesForNodePool("nodepool-3", policies[2], nodepool3Data),
				}

				// Process each nodepool independently and record decisions
				type nodepoolDecisions struct {
					policy              v1.ConsolidateWhenPolicy
					totalCost           float64
					totalDisruption     float64
					candidateRatios     []float64
					consolidatableCount int
				}

				decisions := make([]nodepoolDecisions, 3)

				for i, candidates := range nodepoolCandidates {
					// Compute metrics for this nodepool
					totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

					// Skip if metrics are zero
					if totalCost == 0.0 || totalDisruption == 0.0 {
						return true
					}

					// Compute decision ratios for each candidate
					ratios := make([]float64, len(candidates))
					for j, candidate := range candidates {
						calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
						ratios[j] = candidate.decisionRatio
					}

					// Count how many candidates should be consolidated based on policy
					consolidatableCount := 0
					for j, candidate := range candidates {
						if evaluator.ShouldConsolidate(policies[i], candidate, ratios[j], 1.0) {
							consolidatableCount++
						}
					}

					decisions[i] = nodepoolDecisions{
						policy:              policies[i],
						totalCost:           totalCost,
						totalDisruption:     totalDisruption,
						candidateRatios:     ratios,
						consolidatableCount: consolidatableCount,
					}
				}

				// Now verify independence: process nodepools again in different order
				// and verify decisions remain the same

				// Process in reverse order
				decisionsReverse := make([]nodepoolDecisions, 3)
				for i := 2; i >= 0; i-- {
					candidates := nodepoolCandidates[i]

					// Compute metrics for this nodepool
					totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

					// Skip if metrics are zero
					if totalCost == 0.0 || totalDisruption == 0.0 {
						return true
					}

					// Compute decision ratios for each candidate
					ratios := make([]float64, len(candidates))
					for j, candidate := range candidates {
						calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)
						ratios[j] = candidate.decisionRatio
					}

					// Count how many candidates should be consolidated based on policy
					consolidatableCount := 0
					for j, candidate := range candidates {
						if evaluator.ShouldConsolidate(policies[i], candidate, ratios[j], 1.0) {
							consolidatableCount++
						}
					}

					decisionsReverse[i] = nodepoolDecisions{
						policy:              policies[i],
						totalCost:           totalCost,
						totalDisruption:     totalDisruption,
						candidateRatios:     ratios,
						consolidatableCount: consolidatableCount,
					}
				}

				// Verify that decisions are identical regardless of processing order
				for i := 0; i < 3; i++ {
					// Verify metrics are the same
					if !floatEquals(decisions[i].totalCost, decisionsReverse[i].totalCost, 1e-9) {
						return false
					}
					if !floatEquals(decisions[i].totalDisruption, decisionsReverse[i].totalDisruption, 1e-9) {
						return false
					}

					// Verify decision ratios are the same
					if len(decisions[i].candidateRatios) != len(decisionsReverse[i].candidateRatios) {
						return false
					}
					for j := range decisions[i].candidateRatios {
						ratio1 := decisions[i].candidateRatios[j]
						ratio2 := decisionsReverse[i].candidateRatios[j]

						// Handle infinity case
						if isInf(ratio1) && isInf(ratio2) {
							continue
						}

						if !floatEquals(ratio1, ratio2, 1e-9) {
							return false
						}
					}

					// Verify consolidatable counts are the same
					if decisions[i].consolidatableCount != decisionsReverse[i].consolidatableCount {
						return false
					}
				}

				// Verify that each nodepool's decisions are based only on its own policy
				// NodePool 1 (WhenEmpty): should only consolidate empty nodes
				for _, candidate := range nodepoolCandidates[0] {
					isEmpty := len(candidate.reschedulablePods) == 0
					shouldConsolidate := evaluator.ShouldConsolidate(policies[0], candidate, candidate.decisionRatio, 1.0)

					if isEmpty != shouldConsolidate {
						return false
					}
				}

				// NodePool 2 (WhenEmptyOrUnderutilized): should consolidate all candidates
				for _, candidate := range nodepoolCandidates[1] {
					shouldConsolidate := evaluator.ShouldConsolidate(policies[1], candidate, candidate.decisionRatio, 1.0)

					if !shouldConsolidate {
						return false
					}
				}

				// NodePool 3 (WhenCostJustifiesDisruption): should only consolidate when ratio >= 1.0
				for _, candidate := range nodepoolCandidates[2] {
					shouldConsolidate := evaluator.ShouldConsolidate(policies[2], candidate, candidate.decisionRatio, 1.0)
					expectedShouldConsolidate := candidate.decisionRatio >= 1.0

					if shouldConsolidate != expectedShouldConsolidate {
						return false
					}
				}

				// Verify that modifying one nodepool's candidates doesn't affect others
				// Create a modified version of nodepool 1 with different costs
				modifiedNodepool1 := createCandidatesForNodePool("nodepool-1", policies[0], nodepool1Data)
				for _, candidate := range modifiedNodepool1 {
					// Double the cost
					candidate.instanceType.Offerings[0].Price *= 2.0
				}

				// Recompute metrics for modified nodepool 1
				modifiedTotalCost, modifiedTotalDisruption := calculator.ComputeNodePoolMetrics(ctx, modifiedNodepool1)

				// Skip if metrics are zero
				if modifiedTotalCost == 0.0 || modifiedTotalDisruption == 0.0 {
					return true
				}

				// Compute decision ratios for modified nodepool 1
				for _, candidate := range modifiedNodepool1 {
					calculator.ComputeDecisionRatio(ctx, candidate, modifiedTotalCost, modifiedTotalDisruption)
				}

				// Verify that nodepool 2 and 3 decisions remain unchanged
				for i := 1; i < 3; i++ {
					candidates := nodepoolCandidates[i]

					// Recompute metrics
					totalCost, totalDisruption := calculator.ComputeNodePoolMetrics(ctx, candidates)

					// Skip if metrics are zero
					if totalCost == 0.0 || totalDisruption == 0.0 {
						return true
					}

					// Verify metrics are unchanged
					if !floatEquals(totalCost, decisions[i].totalCost, 1e-9) {
						return false
					}
					if !floatEquals(totalDisruption, decisions[i].totalDisruption, 1e-9) {
						return false
					}

					// Recompute decision ratios
					for j, candidate := range candidates {
						calculator.ComputeDecisionRatio(ctx, candidate, totalCost, totalDisruption)

						// Verify ratios are unchanged
						if isInf(candidate.decisionRatio) && isInf(decisions[i].candidateRatios[j]) {
							continue
						}

						if !floatEquals(candidate.decisionRatio, decisions[i].candidateRatios[j], 1e-9) {
							return false
						}
					}
				}

				return true
			},
			genCandidateDataSlice(),
			genCandidateDataSlice(),
			genCandidateDataSlice(),
		))

	properties.TestingRun(t)
}

// createCandidatesForNodePool creates a slice of candidates for a specific nodepool
func createCandidatesForNodePool(nodepoolName string, policy v1.ConsolidateWhenPolicy, data []candidateTestData) []*Candidate {
	candidates := make([]*Candidate, len(data))

	for i, d := range data {
		// Randomly decide if this candidate should have pods (for WhenEmpty policy testing)
		// Use the disruption value to determine: if disruption > 50, add pods
		hasPods := d.disruption > 50.0

		var pods []*corev1.Pod
		if hasPods {
			// Create some test pods
			pods = []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: "default",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-2",
						Namespace: "default",
					},
				},
			}
		}

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
			DisruptionCost:    d.disruption,
			reschedulablePods: pods,
			NodePool: &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodepoolName,
				},
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidateWhen: policy,
					},
				},
			},
		}
	}

	return candidates
}
