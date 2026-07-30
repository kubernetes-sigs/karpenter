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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

var _ = Describe("Adversarial", func() {
	It("should not allow NaN price to poison NodePool totals", func() {
		np := makeBalancedNodePool("pool-nan", int32Ptr(2))
		pod := makePod("pod", "")

		normalIT := makeInstanceType("m7i.xlarge", 4.84)
		nanIT := &cloudprovider.InstanceType{
			Name: "nan-instance",
			Offerings: cloudprovider.Offerings{&cloudprovider.Offering{
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-zone-1"),
					scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
				),
				Price:     math.NaN(),
				Available: true,
			}},
		}

		candidates := []*Candidate{
			makeCandidate("node-good-1", np, normalIT, []*corev1.Pod{pod}),
			makeCandidate("node-nan", np, nanIT, []*corev1.Pod{pod}),
			makeCandidate("node-good-2", np, normalIT, []*corev1.Pod{pod}),
		}

		totals := computeNodePoolTotals(context.Background(), candidates, candidateNodes(candidates), nil)
		poolTotals := totals[np.Name]

		// NaN should not propagate into totals.
		Expect(math.IsNaN(poolTotals.TotalCost)).To(BeFalse(),
			"NaN propagated into TotalCost: computeNodePoolTotals does not guard against NaN offering prices")

		// ScoreMove must not produce NaN score from poisoned totals.
		result := ScoreMove(4.84, 2.0, poolTotals, 2)
		Expect(math.IsNaN(result.Score())).To(BeFalse(),
			"ScoreMove produced NaN score from poisoned totals; moves are silently rejected forever")
	})

	It("should populate perPool entries for all pools even on early return", func() {
		ctx := options.ToContext(context.Background(), &options.Options{})

		// Pool A: strict (k=1, threshold=1.0) -- will reject a marginal move
		poolA := makeBalancedNodePool("pool-a", int32Ptr(1))
		// Pool B: lenient (k=3, threshold=0.33)
		poolB := makeBalancedNodePool("pool-b", int32Ptr(3))

		itA := makeInstanceType("a-type", 1.0)
		itB := makeInstanceType("b-type", 1.0)

		// Pool A: 2 nodes with heavy pods so score < 1.0
		heavyPods := make([]*corev1.Pod, 20)
		for i := range heavyPods {
			heavyPods[i] = makePod("heavy", "1000")
		}
		poolACandidates := []*Candidate{
			makeCandidate("a-0", poolA, itA, heavyPods),
			makeCandidate("a-1", poolA, itA, []*corev1.Pod{makePod("light", "")}),
		}

		// Pool B: 10 nodes with light pods -- would easily approve
		poolBCandidates := make([]*Candidate, 10)
		for i := range poolBCandidates {
			poolBCandidates[i] = makeCandidate("b-"+string(rune('0'+i)), poolB, itB, []*corev1.Pod{makePod("p", "")})
		}

		allCandidates := append(poolACandidates, poolBCandidates...)
		nodePoolTotals := computeNodePoolTotals(context.Background(), allCandidates, candidateNodes(allCandidates), nil)

		// Command deletes the heavy node from pool A and one node from pool B.
		cmd := Command{
			Candidates: []*Candidate{poolACandidates[0], poolBCandidates[0]},
		}

		_, perPool := EvaluateBalancedMove(ctx, cmd, nodePoolTotals)

		// Both pools should have entries in perPool, regardless of early return.
		for _, poolName := range []string{"pool-a", "pool-b"} {
			result, exists := perPool[poolName]
			Expect(exists).To(BeTrue(),
				"perPool missing entry for %q: downstream event emission will use zero-value ScoreResult (K=0, Threshold=0)", poolName)
			Expect(result.K).ToNot(Equal(0),
				"perPool[%q].K == 0: events will report meaningless k=0 threshold=0", poolName)
		}
	})

	It("should sum candidate costs correctly with nil instanceType", func() {
		np := makeBalancedNodePool("pool-mixed", int32Ptr(2))
		it := makeInstanceType("m7i.xlarge", 4.84)
		pod := makePod("pod", "")

		candidates := []*Candidate{
			makeCandidate("node-good-1", np, it, []*corev1.Pod{pod}),
			makeCandidate("node-nil", np, nil, []*corev1.Pod{pod}), // nil instanceType => Price=0
			makeCandidate("node-good-2", np, it, []*corev1.Pod{pod}),
		}

		// sumCandidatePrices sums Price across all candidates; nil instanceType means Price=0
		totalCost := sumCandidatePrices(candidates)
		Expect(totalCost).To(BeNumerically("~", 9.68, 0.01))
	})

	It("should use source pricing that ignores availability", func() {
		np := makeBalancedNodePool("pool-avail", int32Ptr(2))
		pod := makePod("pod", "")

		// Source instance type: cheapest compatible offering is "unavailable" ($1.00)
		// meaning new launches would fail (ICE). But the node is already running at $1.00.
		sourceIT := &cloudprovider.InstanceType{
			Name: "source-type",
			Offerings: cloudprovider.Offerings{
				&cloudprovider.Offering{
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-zone-1"),
						scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
					),
					Price:     1.00,
					Available: false, // ICE'd -- can't launch new, but running nodes still cost this
				},
				&cloudprovider.Offering{
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-zone-2"),
						scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
					),
					Price:     5.00,
					Available: true,
				},
			},
		}

		candidate := makeCandidate("node-src", np, sourceIT, []*corev1.Pod{pod})

		// Source pricing should use $1.00 -- the node's actual cost, not the cost of
		// a hypothetical new launch.
		sourcePrice := sumCandidatePrices([]*Candidate{candidate})
		expectedPrice := 1.00

		Expect(sourcePrice).To(Equal(expectedPrice),
			"sumCandidatePrices should use $%.2f (node's actual cost) regardless of Available, got $%.2f",
			expectedPrice, sourcePrice)
	})

	It("should approve single-node pool DELETE with expected scoring", func() {
		ctx := context.Background()
		np := makeBalancedNodePool("pool-single", int32Ptr(2))
		it := makeInstanceType("m7i.xlarge", 4.84)
		pod := makePod("pod", "")

		// Pool has exactly 1 node
		candidate := makeCandidate("only-node", np, it, []*corev1.Pod{pod})
		allCandidates := []*Candidate{candidate}
		nodePoolTotals := computeNodePoolTotals(context.Background(), allCandidates, candidateNodes(allCandidates), nil)

		// DELETE the only node
		cmd := Command{
			Candidates: []*Candidate{candidate},
		}

		allApproved, perPool := EvaluateBalancedMove(ctx, cmd, nodePoolTotals)
		result := perPool["pool-single"]

		// savings_fraction = 4.84 / 4.84 = 1.0
		// disruption_fraction = 2.0 / 2.0 = 1.0
		// score = 1.0 / 1.0 = 1.0
		// threshold = 1/2 = 0.5
		// 1.0 >= 0.5 => approved
		Expect(allApproved).To(BeTrue(),
			"single-node DELETE unexpectedly rejected (score=%.2f)", result.Score())

		// Design decision: single-node DELETE scoring 1.0 is accepted behavior.
		// consolidateAfter and disruption budgets are the guards against infinite loops.
		Expect(result.SavingsFraction).To(BeNumerically(">=", 1.0),
			"expected savings_fraction=1.0 for single-node pool DELETE, got %.2f", result.SavingsFraction)
	})
})
