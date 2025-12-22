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

package disruption_test

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// These tests verify the savings threshold feature that prevents marginal consolidation.
//
// The core idea: consolidation should only happen when the hourly savings justify the
// disruption cost. The metric is:
//
//     savingsPerDisruptionCost = (oldPrice - newPrice) / totalDisruptionCost
//
// This represents "dollars saved per hour, per unit of disruption cost." A threshold of 0.01
// means "save at least 1 cent per hour for each unit of disruption cost incurred."
//
// KEY INSIGHT: The ordering of candidates (interweaving across NodePools) is for FAIRNESS -
// ensuring all NodePools get considered. The threshold is for ACCEPTANCE - determining
// whether a specific consolidation move is worth the disruption.

var _ = Describe("Savings Threshold", func() {

	// Helper to create a candidate with a specific disruption cost
	makeCandidate := func(disruptionCost float64) *disruption.Candidate {
		return &disruption.Candidate{DisruptionCost: disruptionCost}
	}

	// Helper to create instance type with offerings at specified prices
	makeInstanceType := func(name string, prices ...float64) *cloudprovider.InstanceType {
		offerings := make([]*cloudprovider.Offering, len(prices))
		for i, price := range prices {
			offerings[i] = &cloudprovider.Offering{
				Available:    true,
				Price:        price,
				Requirements: scheduling.NewRequirements(),
			}
		}
		return &cloudprovider.InstanceType{
			Name:      name,
			Offerings: offerings,
		}
	}

	Context("CheckSavingsThreshold", func() {
		// This function determines whether a consolidation move should proceed based on
		// whether the savings justify the disruption. It implements the formula:
		//
		//     savingsPerDisruptionCost = savings / max(totalDisruptionCost, MinDisruptionCost)
		//
		// The MinDisruptionCost (1.0) handles empty nodes - even with no pods to evict,
		// there's still some cost to the consolidation operation itself.

		DescribeTable("should correctly evaluate threshold",
			func(candidatePrice, replacementPrice, disruptionCost, threshold float64, expectBlock bool) {
				candidates := []*disruption.Candidate{makeCandidate(disruptionCost)}
				config := disruption.ConsolidationDecisionConfig{
					MinSavingsPerDisruptionCost: threshold,
				}

				msg := disruption.CheckSavingsThreshold(candidates, candidatePrice, replacementPrice, config)

				if expectBlock {
					Expect(msg).NotTo(BeEmpty(), "expected consolidation to be blocked")
				} else {
					Expect(msg).To(BeEmpty(), "expected consolidation to proceed")
				}
			},
			// Threshold disabled (0) - always passes regardless of savings
			Entry("threshold disabled (0) always passes",
				1.00, 0.99, 10.0, 0.0, false),

			// Negative threshold treated as disabled (values <= 0 skip the check)
			Entry("negative threshold treated as disabled",
				1.00, 0.99, 10.0, -0.05, false),

			// High savings relative to disruption cost - should pass
			// savings=0.50, ratio=0.50, threshold=0.01 -> 0.50 > 0.01, passes
			Entry("high savings with low disruption cost passes",
				1.00, 0.50, 1.0, 0.01, false),

			// Low savings relative to high disruption cost - should fail
			// savings=0.01, ratio=0.0001, threshold=0.01 -> 0.0001 < 0.01, fails
			Entry("low savings with high disruption cost fails",
				1.00, 0.99, 100.0, 0.01, true),

			// Marginal savings with moderate disruption - should fail
			// savings=0.001, ratio=0.0001, threshold=0.01 -> fails
			Entry("marginal savings with moderate disruption fails",
				0.10, 0.099, 10.0, 0.01, true),

			// Zero disruption cost uses MinDisruptionCost (1.0) - good savings should pass
			// savings=0.50, ratio=0.50/1.0=0.50, threshold=0.01 -> passes
			Entry("zero disruption cost with good savings passes",
				1.00, 0.50, 0.0, 0.01, false),

			// Zero disruption cost uses MinDisruptionCost (1.0) - marginal savings should fail
			// savings=0.001, ratio=0.001/1.0=0.001, threshold=0.01 -> fails
			Entry("zero disruption cost with marginal savings fails",
				1.00, 0.999, 0.0, 0.01, true),

			// Edge case: slightly above threshold - should pass
			// savings=0.11, ratio=0.011, threshold=0.01 -> passes
			Entry("slightly above threshold passes",
				1.00, 0.89, 10.0, 0.01, false),

			// Edge case: slightly below threshold - should fail
			// savings=0.08, ratio=0.008, threshold=0.01 -> fails
			Entry("slightly below threshold fails",
				1.00, 0.92, 10.0, 0.01, true),

			// No savings (equal prices) - passes because no savings check needed
			// This is typically caught elsewhere, but threshold check should not block
			Entry("no savings (equal prices) passes",
				1.00, 1.00, 10.0, 0.01, false),

			// Negative savings (replacement more expensive) - passes
			// This is caught elsewhere; threshold check only cares about positive savings
			Entry("negative savings passes",
				1.00, 1.10, 10.0, 0.01, false),
		)
	})

	Context("GetTotalDisruptionCost", func() {
		// This helper sums the disruption costs of all candidates. Used to calculate
		// the denominator in the savings-per-disruption-cost ratio.

		It("should sum costs for single candidate", func() {
			candidates := []*disruption.Candidate{makeCandidate(5.0)}
			Expect(disruption.GetTotalDisruptionCost(candidates)).To(Equal(5.0))
		})

		It("should sum costs for multiple candidates", func() {
			candidates := []*disruption.Candidate{
				makeCandidate(1.0),
				makeCandidate(2.0),
				makeCandidate(3.0),
			}
			Expect(disruption.GetTotalDisruptionCost(candidates)).To(Equal(6.0))
		})

		It("should return zero for empty candidates", func() {
			candidates := []*disruption.Candidate{}
			Expect(disruption.GetTotalDisruptionCost(candidates)).To(Equal(0.0))
		})

		It("should handle zero cost candidates", func() {
			candidates := []*disruption.Candidate{
				makeCandidate(0.0),
				makeCandidate(0.0),
			}
			Expect(disruption.GetTotalDisruptionCost(candidates)).To(Equal(0.0))
		})
	})

	Context("MinDisruptionCost constant", func() {
		// MinDisruptionCost prevents divide-by-zero for empty nodes and ensures that
		// even nodes with no pods require some minimum savings to justify consolidation.
		// Set to 1.0, equivalent to the cost of evicting one default pod.

		It("should be positive", func() {
			Expect(disruption.MinDisruptionCost).To(BeNumerically(">", 0))
		})

		It("should equal 1.0 (equivalent to one default pod)", func() {
			Expect(disruption.MinDisruptionCost).To(Equal(1.0))
		})
	})

	Context("GetCheapestReplacementPrice", func() {
		// This helper finds the cheapest available offering price for the first instance type.
		// PRECONDITION: instance types must be sorted by price (via OrderByPrice).
		// The function only looks at the first instance type since it should be the cheapest.

		It("should return cheapest price from first instance type", func() {
			// Precondition: instance types must be sorted by price
			instanceTypes := cloudprovider.InstanceTypes{
				makeInstanceType("cheap", 0.10, 0.15),   // cheapest type
				makeInstanceType("expensive", 0.50),     // more expensive type
			}
			reqs := scheduling.NewRequirements()

			Expect(disruption.GetCheapestReplacementPrice(instanceTypes, reqs)).To(Equal(0.10))
		})

		It("should return cheapest offering from first type when multiple offerings exist", func() {
			// First type has multiple offerings at different prices
			instanceTypes := cloudprovider.InstanceTypes{
				makeInstanceType("first", 0.20, 0.10, 0.15), // cheapest offering is 0.10
				makeInstanceType("second", 0.05),            // cheaper type, but we only check first
			}
			reqs := scheduling.NewRequirements()

			// Returns 0.10 (cheapest offering of first type), not 0.05 (second type)
			Expect(disruption.GetCheapestReplacementPrice(instanceTypes, reqs)).To(Equal(0.10))
		})

		It("should return MaxFloat64 for empty instance types", func() {
			instanceTypes := cloudprovider.InstanceTypes{}
			reqs := scheduling.NewRequirements()

			Expect(disruption.GetCheapestReplacementPrice(instanceTypes, reqs)).To(Equal(math.MaxFloat64))
		})

		It("should return MaxFloat64 when no offerings are available", func() {
			offerings := []*cloudprovider.Offering{
				{Available: false, Price: 0.10, Requirements: scheduling.NewRequirements()},
			}
			instanceTypes := cloudprovider.InstanceTypes{
				{Name: "unavailable", Offerings: offerings},
			}
			reqs := scheduling.NewRequirements()

			Expect(disruption.GetCheapestReplacementPrice(instanceTypes, reqs)).To(Equal(math.MaxFloat64))
		})
	})

	Context("GetMinSavingsPerDisruptionCost", func() {
		// This function determines which threshold to use for multi-node consolidation.
		//
		// KEY DESIGN DECISION: Use the MAXIMUM threshold across all candidates.
		//
		// When consolidating multiple nodes from different NodePools, each NodePool might
		// have a different threshold. For the consolidation to proceed, it must meet the
		// threshold of EVERY source node's NodePool. Using the max ensures that if any
		// NodePool requires a high bar, the whole consolidation must meet it.
		//
		// Example: NodePool A has threshold 0.01, NodePool B has threshold 0.05.
		// A multi-node consolidation involving nodes from both must achieve ratio >= 0.05.

		// Helper to create a NodePool with a specific threshold
		makeNodePool := func(_ string, threshold *string) *v1.NodePool {
			return &v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						MinSavingsPerDisruptionCost: threshold,
					},
				},
			}
		}

		// Helper to create a candidate with a specific NodePool
		makeCandidateWithNodePool := func(nodePool *v1.NodePool) *disruption.Candidate {
			return &disruption.Candidate{
				NodePool: nodePool,
			}
		}

		strPtr := func(s string) *string { return &s }

		It("should return controller default when no candidates", func() {
			candidates := []*disruption.Candidate{}
			// Controller default is 0 based on our options setup
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.0))
		})

		It("should return controller default when candidates have nil NodePool", func() {
			candidates := []*disruption.Candidate{
				{NodePool: nil},
				{NodePool: nil},
			}
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.0))
		})

		It("should return controller default when NodePool has no threshold set", func() {
			nodePool := makeNodePool("test", nil)
			candidates := []*disruption.Candidate{
				makeCandidateWithNodePool(nodePool),
			}
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.0))
		})

		It("should return NodePool threshold for single candidate", func() {
			nodePool := makeNodePool("test", strPtr("0.02"))
			candidates := []*disruption.Candidate{
				makeCandidateWithNodePool(nodePool),
			}
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.02))
		})

		It("should return max threshold for multi-node with same NodePool", func() {
			// Two candidates from same NodePool - should use that NodePool's threshold
			nodePool := makeNodePool("shared", strPtr("0.03"))
			candidates := []*disruption.Candidate{
				makeCandidateWithNodePool(nodePool),
				makeCandidateWithNodePool(nodePool),
			}
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.03))
		})

		It("should return max threshold for multi-node with different NodePools", func() {
			// Two candidates from different NodePools with different thresholds
			// Should use the HIGHER threshold (most restrictive)
			lowThresholdPool := makeNodePool("low", strPtr("0.01"))
			highThresholdPool := makeNodePool("high", strPtr("0.05"))
			candidates := []*disruption.Candidate{
				makeCandidateWithNodePool(lowThresholdPool),
				makeCandidateWithNodePool(highThresholdPool),
			}
			// Max of 0.01 and 0.05 = 0.05
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.05))
		})

		It("should use controller default when it's higher than NodePool thresholds", func() {
			// If controller default is 0.10 but NodePool only sets 0.02,
			// we should still use max(0.10, 0.02) = 0.10
			// But our test context has controller default of 0, so this tests the other direction
			nodePool := makeNodePool("low", strPtr("0.01"))
			candidates := []*disruption.Candidate{
				makeCandidateWithNodePool(nodePool),
			}
			// Controller default is 0, NodePool is 0.01, max = 0.01
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.01))
		})

		It("should handle mixed candidates (some with threshold, some without)", func() {
			// One NodePool has threshold, another doesn't
			withThreshold := makeNodePool("with", strPtr("0.04"))
			withoutThreshold := makeNodePool("without", nil)
			candidates := []*disruption.Candidate{
				makeCandidateWithNodePool(withThreshold),
				makeCandidateWithNodePool(withoutThreshold),
			}
			// Should use 0.04 (the only specified threshold)
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.04))
		})

		It("should handle invalid threshold strings gracefully", func() {
			// If threshold string can't be parsed, skip it
			invalidThreshold := makeNodePool("invalid", strPtr("not-a-number"))
			validThreshold := makeNodePool("valid", strPtr("0.02"))
			candidates := []*disruption.Candidate{
				makeCandidateWithNodePool(invalidThreshold),
				makeCandidateWithNodePool(validThreshold),
			}
			// Should use 0.02 (skip the invalid one)
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.02))
		})

		It("should handle threshold of 0 (disabled)", func() {
			// Threshold of 0 means disabled - but we still use max
			zeroThreshold := makeNodePool("zero", strPtr("0"))
			someThreshold := makeNodePool("some", strPtr("0.01"))
			candidates := []*disruption.Candidate{
				makeCandidateWithNodePool(zeroThreshold),
				makeCandidateWithNodePool(someThreshold),
			}
			// Max of 0 and 0.01 = 0.01
			Expect(disruption.GetMinSavingsPerDisruptionCost(ctx, candidates)).To(Equal(0.01))
		})
	})
})
