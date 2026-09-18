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
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
)

var _ = Describe("ScoreMove", func() {
	// RFC example pool: 10 nodes, eight m7i.xlarge ($4.84/day) and two m7i.2xlarge ($9.68/day).
	// Total NodePool cost = $58.08/day. 80 pods, total disruption cost = 90 (10 nodes * 1.0 + 80 pods * 1.0).
	var defaultTotals = disruption.NodePoolTotals{
		TotalCost:           58.08,
		TotalDisruptionCost: 90,
	}

	Context("when the move has clear savings", func() {
		It("should approve an oversized node consolidation", func() {
			savings := 7.26       // m7i.2xlarge - m7i.large = 9.68 - 2.42
			disruptionCost := 4.0 // 1.0 per-node + 3 pods with default cost
			result := disruption.ScoreMove(savings, disruptionCost, defaultTotals, v1.BalancedK)

			expectedSavingsFrac := savings / defaultTotals.TotalCost
			expectedDisruptionFrac := disruptionCost / defaultTotals.TotalDisruptionCost
			expectedScore := expectedSavingsFrac / expectedDisruptionFrac

			Expect(result.Approved()).To(BeTrue(), "score=%.2f", result.Score())
			Expect(result.Score()).To(BeNumerically("~", expectedScore, 0.02))
			Expect(result.SavingsFraction).To(BeNumerically("~", expectedSavingsFrac, 0.001))
			Expect(result.DisruptionFraction).To(BeNumerically("~", expectedDisruptionFrac, 0.001))
		})

		It("should approve a spare capacity delete", func() {
			savings := 4.84       // full m7i.xlarge cost
			disruptionCost := 5.0 // 1.0 per-node + 4 pods
			result := disruption.ScoreMove(savings, disruptionCost, defaultTotals, v1.BalancedK)

			expectedScore := (savings / defaultTotals.TotalCost) / (disruptionCost / defaultTotals.TotalDisruptionCost)

			Expect(result.Approved()).To(BeTrue(), "score=%.2f", result.Score())
			Expect(result.Score()).To(BeNumerically("~", expectedScore, 0.02))
		})
	})

	Context("when the move is marginal or harmful", func() {
		It("should reject a marginal move", func() {
			savings := 2.42       // m7i.xlarge - m7i.large
			disruptionCost := 9.0 // 1.0 per-node + 8 pods
			result := disruption.ScoreMove(savings, disruptionCost, defaultTotals, v1.BalancedK)

			expectedScore := (savings / defaultTotals.TotalCost) / (disruptionCost / defaultTotals.TotalDisruptionCost)

			Expect(result.Approved()).To(BeFalse(), "score=%.2f", result.Score())
			Expect(result.Score()).To(BeNumerically("~", expectedScore, 0.01))
		})

		It("should reject a well-packed node with zero savings", func() {
			savings := 0.0
			disruptionCost := 11.0 // 1.0 per-node + 10 pods
			result := disruption.ScoreMove(savings, disruptionCost, defaultTotals, v1.BalancedK)

			Expect(result.Approved()).To(BeFalse(), "score=%.2f", result.Score())
			Expect(result.Score()).To(BeZero())
		})
	})

	Context("when the pool is uniform", func() {
		It("should approve a replace in a uniform pool", func() {
			uniformTotals := disruption.NodePoolTotals{
				TotalCost:           48.40,
				TotalDisruptionCost: 90, // 10 nodes + 80 pods
			}
			savings := 2.42       // m7i.xlarge - m7i.large
			disruptionCost := 9.0 // 1.0 per-node + 8 pods
			result := disruption.ScoreMove(savings, disruptionCost, uniformTotals, v1.BalancedK)

			expectedScore := (savings / uniformTotals.TotalCost) / (disruptionCost / uniformTotals.TotalDisruptionCost)

			Expect(result.Approved()).To(BeTrue(), "score=%.2f", result.Score())
			Expect(result.Score()).To(BeNumerically("~", expectedScore, 0.01))
		})
	})

	Context("when disruption costs vary across nodes", func() {
		It("should approve low-disruption nodes and reject high-disruption nodes", func() {
			// 10 nodes * 1.0 + 107 pod disruption = 117
			hetTotals := disruption.NodePoolTotals{
				TotalCost:           58.08,
				TotalDisruptionCost: 117,
			}

			// Node A: disruption = 1.0 (node) + 4.0 (pods) = 5.0
			savingsA := 4.84
			disruptionA := 5.0
			expectedA := (savingsA / hetTotals.TotalCost) / (disruptionA / hetTotals.TotalDisruptionCost)
			resultA := disruption.ScoreMove(savingsA, disruptionA, hetTotals, v1.BalancedK)
			Expect(resultA.Approved()).To(BeTrue(), "Node A score=%.2f", resultA.Score())
			Expect(resultA.Score()).To(BeNumerically("~", expectedA, 0.02))

			// Node B: disruption = 1.0 (node) + 31.0 (pods) = 32.0
			savingsB := 4.84
			disruptionB := 32.0
			expectedB := (savingsB / hetTotals.TotalCost) / (disruptionB / hetTotals.TotalDisruptionCost)
			resultB := disruption.ScoreMove(savingsB, disruptionB, hetTotals, v1.BalancedK)
			Expect(resultB.Approved()).To(BeFalse(), "Node B score=%.2f", resultB.Score())
			Expect(resultB.Score()).To(BeNumerically("~", expectedB, 0.01))
		})
	})

	Context("scale invariance", func() {
		It("should produce the same score regardless of pool size", func() {
			savings := 7.26
			disruptionCost := 4.0

			result10 := disruption.ScoreMove(savings, disruptionCost, defaultTotals, v1.BalancedK)

			scaledTotals := disruption.NodePoolTotals{
				TotalCost:           580.80,
				TotalDisruptionCost: 900, // 100 nodes + 800 pods
			}
			result100 := disruption.ScoreMove(savings, disruptionCost, scaledTotals, v1.BalancedK)

			expectedScore := (savings / defaultTotals.TotalCost) / (disruptionCost / defaultTotals.TotalDisruptionCost)

			Expect(result10.Score()).To(BeNumerically("~", result100.Score(), 0.01))
			Expect(result10.Score()).To(BeNumerically("~", expectedScore, 0.02))
		})
	})

	Context("edge cases with zero values", func() {
		It("should approve with +Inf score when move disruption is zero and savings positive", func() {
			result := disruption.ScoreMove(4.84, 0.0, defaultTotals, v1.BalancedK)
			Expect(result.Approved()).To(BeTrue())
			Expect(math.IsInf(result.Score(), 1)).To(BeTrue())
		})

		It("should reject when both savings and disruption are zero", func() {
			result := disruption.ScoreMove(0.0, 0.0, defaultTotals, v1.BalancedK)
			Expect(result.Approved()).To(BeFalse())
		})

		It("should reject when nodepool cost is zero", func() {
			zeroTotals := disruption.NodePoolTotals{TotalCost: 0, TotalDisruptionCost: 90}
			result := disruption.ScoreMove(4.84, 5.0, zeroTotals, v1.BalancedK)
			Expect(result.Approved()).To(BeFalse())
		})

		It("should approve when move disruption is zero (division by zero guard)", func() {
			result := disruption.ScoreMove(4.84, 0.0, defaultTotals, v1.BalancedK)
			Expect(result.Approved()).To(BeTrue())
		})
	})

	Context("consolidation threshold tuning", func() {
		It("should reject at k=2 for a marginal move", func() {
			result := disruption.ScoreMove(2.42, 9.0, defaultTotals, 2)
			Expect(result.Approved()).To(BeFalse(), "k=2: score=%.2f, threshold=0.50", result.Score())
		})

		It("should approve at k=3 for the same move", func() {
			result := disruption.ScoreMove(2.42, 9.0, defaultTotals, 3)
			Expect(result.Approved()).To(BeTrue(), "k=3: score=%.2f, threshold=0.33", result.Score())
		})

		It("should reject at k=1 for the same move", func() {
			result := disruption.ScoreMove(2.42, 9.0, defaultTotals, 1)
			Expect(result.Approved()).To(BeFalse(), "k=1: score=%.2f, threshold=1.00", result.Score())
		})
	})

	Context("cross-NodePool scoring", func() {
		It("should produce identical scores for proportionally equivalent moves", func() {
			odTotals := disruption.NodePoolTotals{TotalCost: 48.40, TotalDisruptionCost: 90}
			spotTotals := disruption.NodePoolTotals{TotalCost: 14.50, TotalDisruptionCost: 90}

			odResult := disruption.ScoreMove(4.84, 4.0, odTotals, v1.BalancedK)
			spotResult := disruption.ScoreMove(1.45, 4.0, spotTotals, v1.BalancedK)

			Expect(odResult.Score()).To(BeNumerically("~", spotResult.Score(), 0.01))
			Expect(odResult.Approved()).To(BeTrue())
			Expect(spotResult.Approved()).To(BeTrue())
		})
	})
})

var _ = Describe("ConsolidationPolicy.IsBalanced", func() {
	It("should be true for Balanced policy", func() {
		Expect(v1.ConsolidationPolicyBalanced.IsBalanced()).To(BeTrue())
	})

	It("should be false for WhenEmptyOrUnderutilized policy", func() {
		Expect(v1.ConsolidationPolicyWhenEmptyOrUnderutilized.IsBalanced()).To(BeFalse())
	})

	It("should be false for WhenEmpty policy", func() {
		Expect(v1.ConsolidationPolicyWhenEmpty.IsBalanced()).To(BeFalse())
	})
})
