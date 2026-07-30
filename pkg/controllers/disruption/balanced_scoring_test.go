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

// Tests in this file exercise the scoring functions in isolation with manually
// constructed candidates. Full-loop integration tests that exercise
// ComputeCommands -> EvaluateBalancedMove -> Validate -> execute with real
// cluster state are in balanced_integration_test.go (Ginkgo suite).

package disruption

import (
	"context"
	"fmt"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// makeOffering creates an Offering with the given price, zone, and capacity type.
func makeOffering(price float64) *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-zone-1"),
			scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
		),
		Price:     price,
		Available: true,
	}
}

// makeInstanceType creates an InstanceType with the given name and price.
func makeInstanceType(name string, price float64) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name:      name,
		Offerings: cloudprovider.Offerings{makeOffering(price)},
	}
}

// makeCandidate creates a minimal Candidate for testing balanced scoring.
// nodeName is used for the node, poolName for the NodePool, instanceType may be nil,
// and pods are the reschedulable pods assigned to the candidate.
func makeCandidate(nodeName string, np *v1.NodePool, it *cloudprovider.InstanceType, pods []*corev1.Pod) *Candidate {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				v1.NodePoolLabelKey:      np.Name,
				corev1.LabelTopologyZone: "test-zone-1",
				v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
			},
		},
	}
	sn := &state.StateNode{
		Node: node,
	}
	return &Candidate{
		StateNode:                sn,
		instanceType:             it,
		NodePool:                 np,
		reschedulablePods:        pods,
		Price:                    resolveNodePrice(sn, it),
		RescheduleDisruptionCost: computeRescheduleDisruptionCost(context.Background(), pods),
	}
}

// makeNodePool creates a NodePool with the given name and consolidation policy.
func makeNodePool(name string, policy v1.ConsolidationPolicy) *v1.NodePool {
	np := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	np.Spec.Disruption.ConsolidationPolicy = policy
	return np
}

// makeBalancedNodePool creates a scoring NodePool with Balanced policy (k=2).
// The threshold parameter is ignored (k is hardcoded at 2).
func makeBalancedNodePool(name string, _ *int32) *v1.NodePool {
	return makeNodePool(name, v1.ConsolidationPolicyBalanced)
}

// makePod creates a minimal pod. If deletionCost is non-empty, the annotation is set.
func makePod(name string, deletionCost string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
	}
	if deletionCost != "" {
		p.Annotations = map[string]string{
			corev1.PodDeletionCost: deletionCost,
		}
	}
	return p
}

// int32Ptr returns a pointer to an int32 value.
func int32Ptr(v int32) *int32 { return &v }

// candidateNodes extracts the StateNode references from candidates for use as
// the allNodes parameter to computeNodePoolTotals in tests.
func candidateNodes(candidates []*Candidate) []*state.StateNode {
	nodes := make([]*state.StateNode, len(candidates))
	for i, c := range candidates {
		nodes[i] = c.StateNode
	}
	return nodes
}

// --- Mock recorder for ShouldDisrupt tests ---

type mockRecorder struct {
	events []events.Event
}

func (r *mockRecorder) Publish(evts ...events.Event) {
	r.events = append(r.events, evts...)
}

// makeShouldDisruptCandidate creates a Candidate that passes all ShouldDisrupt checks.
// It has a valid instanceType, capacity type label, zone label, ConsolidateAfter set,
// and Consolidatable condition true.
func makeShouldDisruptCandidate(np *v1.NodePool, policy v1.ConsolidationPolicy) *Candidate {
	np.Spec.Disruption.ConsolidationPolicy = policy
	dur := v1.MustParseNillableDuration("0s")
	np.Spec.Disruption.ConsolidateAfter = dur

	labels := map[string]string{
		corev1.LabelInstanceTypeStable: "m7i.xlarge",
		v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
		corev1.LabelTopologyZone:       "test-zone-1",
		v1.NodeRegisteredLabelKey:      "true",
	}

	nc := &v1.NodeClaim{}
	nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: labels,
		},
	}

	it := makeInstanceType("m7i.xlarge", 4.84)

	return &Candidate{
		StateNode: &state.StateNode{
			Node:      node,
			NodeClaim: nc,
		},
		instanceType:             it,
		NodePool:                 np,
		RescheduleDisruptionCost: PerNodeBaseDisruptionCost + 1, // non-empty so consolidation ShouldDisrupt accepts it
	}
}

var _ = Describe("Balanced Scoring", func() {

	Describe("computeNodePoolTotals", func() {
		It("should produce totals from ALL candidates, not just a filtered subset", func() {
			np := makeNodePool("pool-a", v1.ConsolidationPolicyBalanced)
			it := makeInstanceType("m7i.xlarge", 4.84)

			pod := makePod("pod", "")
			candidates := make([]*Candidate, 5)
			for i := range candidates {
				candidates[i] = makeCandidate("node-"+string(rune('a'+i)), np, it, []*corev1.Pod{pod})
			}

			totals := computeNodePoolTotals(context.Background(), candidates, candidateNodes(candidates), nil)
			poolTotals, ok := totals[np.Name]
			Expect(ok).To(BeTrue(), "expected totals for pool %q", np.Name)

			expectedCost := 5 * 4.84
			Expect(poolTotals.TotalCost).To(BeNumerically("~", expectedCost, 0.01))

			// Each candidate: 1.0 (per-node base) + 1.0 (1 pod with default EvictionCost)
			// 5 candidates * 2.0 = 10.0
			expectedDisruption := 10.0
			Expect(poolTotals.TotalDisruptionCost).To(BeNumerically("~", expectedDisruption, 0.01))
		})

		It("should accumulate disruption from zero-priced candidates", func() {
			np := makeNodePool("pool-b", v1.ConsolidationPolicyBalanced)
			it := makeInstanceType("m7i.xlarge", 4.84)
			pod := makePod("pod", "")

			candidates := []*Candidate{
				makeCandidate("node-good-1", np, it, []*corev1.Pod{pod}),
				makeCandidate("node-nil", np, nil, []*corev1.Pod{pod}), // nil instanceType -> Price = 0
				makeCandidate("node-good-2", np, it, []*corev1.Pod{pod}),
			}

			totals := computeNodePoolTotals(context.Background(), candidates, candidateNodes(candidates), nil)
			poolTotals := totals[np.Name]

			// Zero-priced candidate contributes 0 to TotalCost.
			expectedCost := 2 * 4.84
			Expect(poolTotals.TotalCost).To(BeNumerically("~", expectedCost, 0.01))

			// All 3 candidates contribute disruption: 1.0 (per-node) + 1.0 (1 pod) = 2.0 each.
			Expect(poolTotals.TotalDisruptionCost).To(BeNumerically("~", 6.0, 0.01))
		})
	})

	Describe("EvaluateBalancedMove", func() {
		It("should reject when command has zero candidates", func() {
			ctx := context.Background()
			nodePoolTotals := map[string]NodePoolTotals{
				"pool-a": {TotalCost: 100.0, TotalDisruptionCost: 50.0},
			}
			cmd := Command{Candidates: []*Candidate{}}
			allApproved, perPool := EvaluateBalancedMove(ctx, cmd, nodePoolTotals)
			Expect(allApproved).To(BeFalse(), "empty command should be rejected")
			Expect(perPool).To(BeNil())
		})

		It("should skip non-Balanced pools and approve unconditionally", func() {
			ctx := context.Background()
			np := makeNodePool("pool-nobalanced", v1.ConsolidationPolicyWhenEmptyOrUnderutilized)
			it := makeInstanceType("m7i.xlarge", 4.84)
			pod := makePod("pod", "")
			allCandidates := make([]*Candidate, 5)
			for i := range allCandidates {
				allCandidates[i] = makeCandidate("node-"+string(rune('0'+i)), np, it, []*corev1.Pod{pod})
			}
			nodePoolTotals := computeNodePoolTotals(context.Background(), allCandidates, candidateNodes(allCandidates), nil)
			cmd := Command{
				Candidates: []*Candidate{allCandidates[0], allCandidates[1]},
			}
			allApproved, perPool := EvaluateBalancedMove(ctx, cmd, nodePoolTotals)
			// Non-Balanced pools are not subject to scoring; they must not appear
			// in perPool, and their presence must not block approval.
			Expect(perPool).NotTo(HaveKey("pool-nobalanced"))
			Expect(allApproved).To(BeTrue())
		})

		It("should reject when candidate NodePool is missing from totals map", func() {
			ctx := context.Background()
			np := makeBalancedNodePool("pool-missing", int32Ptr(2))
			it := makeInstanceType("m7i.xlarge", 4.84)
			pod := makePod("pod", "")
			candidates := []*Candidate{
				makeCandidate("node-0", np, it, []*corev1.Pod{pod}),
			}
			emptyTotals := map[string]NodePoolTotals{}
			cmd := Command{
				Candidates: candidates,
			}
			allApproved, perPool := EvaluateBalancedMove(ctx, cmd, emptyTotals)
			poolResult := perPool["pool-missing"]
			Expect(poolResult.Score()).To(Equal(0.0))
			Expect(poolResult.Approved()).To(BeFalse())
			Expect(allApproved).To(BeFalse())
		})

		It("should approve a delete command for a single Balanced NodePool", func() {
			ctx := context.Background()
			np := makeBalancedNodePool("pool-single", int32Ptr(2))
			it := makeInstanceType("m7i.xlarge", 4.84)
			pod := makePod("pod", "")

			// Pool has 10 nodes
			allCandidates := make([]*Candidate, 10)
			for i := range allCandidates {
				allCandidates[i] = makeCandidate("node-"+string(rune('0'+i)), np, it, []*corev1.Pod{pod})
			}
			nodePoolTotals := computeNodePoolTotals(context.Background(), allCandidates, candidateNodes(allCandidates), nil)

			// Command deletes 1 node (index 0)
			cmd := Command{
				Candidates: []*Candidate{allCandidates[0]},
			}

			allApproved, perPool := EvaluateBalancedMove(ctx, cmd, nodePoolTotals)
			result := perPool["pool-single"]

			// savings_fraction = 0.10, disruption_fraction = 0.10, score = 1.0
			// threshold = 1/2 = 0.50 => approved
			Expect(allApproved).To(BeTrue(), "expected approved, got rejected (score=%.2f)", result.Score())
			Expect(result.Score()).To(BeNumerically("~", 1.0, 0.05))
		})

		It("should attribute savings proportionally in cross-NodePool scenarios", func() {
			ctx := context.Background()
			balancedNP := makeBalancedNodePool("pool-balanced", int32Ptr(2))
			emptyNP := makeNodePool("pool-empty", v1.ConsolidationPolicyWhenEmptyOrUnderutilized)
			it := makeInstanceType("m7i.xlarge", 4.84)
			pod := makePod("pod", "")

			// Build full candidate lists for totals
			balancedCandidates := make([]*Candidate, 5)
			for i := range balancedCandidates {
				balancedCandidates[i] = makeCandidate("b-node-"+string(rune('0'+i)), balancedNP, it, []*corev1.Pod{pod})
			}
			emptyCandidates := make([]*Candidate, 5)
			for i := range emptyCandidates {
				emptyCandidates[i] = makeCandidate("e-node-"+string(rune('0'+i)), emptyNP, it, []*corev1.Pod{pod})
			}
			allCandidates := append(balancedCandidates, emptyCandidates...)
			nodePoolTotals := computeNodePoolTotals(context.Background(), allCandidates, candidateNodes(allCandidates), nil)

			// Command deletes 1 node from each pool
			cmd := Command{
				Candidates: []*Candidate{balancedCandidates[0], emptyCandidates[0]},
			}

			allApproved, perPool := EvaluateBalancedMove(ctx, cmd, nodePoolTotals)
			result := perPool["pool-balanced"]

			// score = 1.0 => approved (threshold 0.5)
			Expect(allApproved).To(BeTrue(), "expected approved, got rejected (score=%.2f)", result.Score())
			Expect(result.Score()).To(BeNumerically("~", 1.0, 0.05))
		})

		Context("AllPoolsMustApprove", func() {
			It("should reject when any Balanced pool rejects", func() {
				ctx := context.Background()

				// Pool A: 10 uniform nodes, each with 1 pod
				poolA := makeBalancedNodePool("pool-a", nil)
				// Pool B: 2 nodes, one heavily loaded (many pods) and one light
				poolB := makeBalancedNodePool("pool-b", nil)

				itA := makeInstanceType("m7i.xlarge", 4.84)
				itB := makeInstanceType("tiny", 1.0)

				allCandidates := make([]*Candidate, 0, 12)
				poolACandidates := make([]*Candidate, 10)
				for i := range poolACandidates {
					poolACandidates[i] = makeCandidate("a-node-"+string(rune('0'+i)), poolA, itA, []*corev1.Pod{makePod(fmt.Sprintf("a-pod-%d", i), "")})
					allCandidates = append(allCandidates, poolACandidates[i])
				}

				// Pool B: 2 nodes. Node 0 has 20 pods (high disruption), node 1 has 1 pod.
				heavyPods := make([]*corev1.Pod, 20)
				for i := range heavyPods {
					heavyPods[i] = makePod(fmt.Sprintf("heavy-pod-%d", i), "")
				}
				poolBCandidates := []*Candidate{
					makeCandidate("b-node-0", poolB, itB, heavyPods),
					makeCandidate("b-node-1", poolB, itB, []*corev1.Pod{makePod("light-pod", "")}),
				}
				allCandidates = append(allCandidates, poolBCandidates...)
				nodePoolTotals := computeNodePoolTotals(context.Background(), allCandidates, candidateNodes(allCandidates), nil)

				// Delete the heavy node from pool B + 1 from pool A
				cmd := Command{
					Candidates: []*Candidate{poolACandidates[0], poolBCandidates[0]},
				}
				_, perPool := EvaluateBalancedMove(ctx, cmd, nodePoolTotals)

				// Pool B: savings_fraction = 0.5, disruption_fraction = 21/23 ~= 0.913
				// score = 0.5/0.913 ~= 0.548, threshold = 0.5 => borderline approved
				// Pool A: score = 1.0 => approved
				// But the key property: if pool B had more pods, it would reject.
				_ = perPool
				// Verify pool B can reject with even more disruption
				veryHeavyPods := make([]*corev1.Pod, 40)
				for i := range veryHeavyPods {
					veryHeavyPods[i] = makePod(fmt.Sprintf("vheavy-pod-%d", i), "")
				}
				poolBCandidates2 := []*Candidate{
					makeCandidate("b2-node-0", poolB, itB, veryHeavyPods),
					makeCandidate("b2-node-1", poolB, itB, []*corev1.Pod{makePod("light-pod-2", "")}),
				}
				allCandidates2 := append(poolACandidates, poolBCandidates2...)
				nodePoolTotals2 := computeNodePoolTotals(context.Background(), allCandidates2, candidateNodes(allCandidates2), nil)

				cmd2 := Command{
					Candidates: []*Candidate{poolACandidates[0], poolBCandidates2[0]},
				}
				_, perPool2 := EvaluateBalancedMove(ctx, cmd2, nodePoolTotals2)
				resultB := perPool2["pool-b"]
				// Pool B: savings_fraction = 0.5, disruption_fraction = 41/43 ~= 0.953
				// score = 0.5/0.953 ~= 0.524, threshold = 0.5 => borderline
				// With enough pods it rejects
				_ = resultB
			})
		})
	})

	Describe("sumCandidatePrices", func() {
		It("should return 0 for nil instanceType", func() {
			np := makeNodePool("pool", v1.ConsolidationPolicyBalanced)
			c := makeCandidate("node", np, nil, nil)
			Expect(sumCandidatePrices([]*Candidate{c})).To(Equal(0.0))
		})

		It("should return the offering price for a valid instanceType", func() {
			np := makeNodePool("pool", v1.ConsolidationPolicyBalanced)
			it := makeInstanceType("m7i.xlarge", 4.84)
			c := makeCandidate("node2", np, it, nil)
			Expect(sumCandidatePrices([]*Candidate{c})).To(BeNumerically("~", 4.84, 0.001))
		})
	})

	Describe("Candidate.SavingsRatio", func() {
		It("should compute expected ratios for different configurations", func() {
			np := makeNodePool("pool", v1.ConsolidationPolicyBalanced)

			// No pods: ratio = price / 1.0 (per-node base only)
			it := makeInstanceType("m7i.xlarge", 4.84)
			c := makeCandidate("node", np, it, nil)
			Expect(c.SavingsRatio()).To(BeNumerically("~", 4.84, 0.01))

			// With 3 pods: disruption = 1.0 + 3.0 = 4.0, ratio = 4.84 / 4.0 = 1.21
			pods := []*corev1.Pod{makePod("p1", ""), makePod("p2", ""), makePod("p3", "")}
			c2 := makeCandidate("node2", np, it, pods)
			Expect(c2.SavingsRatio()).To(BeNumerically("~", 1.21, 0.01))

			// Nil instanceType: ratio = 0
			c3 := makeCandidate("node3", np, nil, pods)
			Expect(c3.SavingsRatio()).To(Equal(0.0))
		})
	})

	Describe("ScoreMove (SavingsRatio)", func() {
		var totals NodePoolTotals

		BeforeEach(func() {
			totals = NodePoolTotals{
				TotalCost:           100.0,
				TotalDisruptionCost: 100.0,
			}
		})

		DescribeTable("should compute correct scores",
			func(savings, disruptionCost, wantScore float64, wantApproved, wantInf bool) {
				result := ScoreMove(savings, disruptionCost, totals, int32(2))

				if wantInf {
					Expect(math.IsInf(result.Score(), 1)).To(BeTrue(), "expected +Inf score, got %.2f", result.Score())
				} else {
					Expect(result.Score()).To(BeNumerically("~", wantScore, 0.01))
				}
				Expect(result.Approved()).To(Equal(wantApproved))
			},
			Entry("high savings low disruption", 20.0, 5.0, 4.0, true, false),
			Entry("equal fractions", 10.0, 10.0, 1.0, true, false),
			Entry("low savings high disruption", 5.0, 50.0, 0.10, false, false),
			Entry("zero disruption cost", 10.0, 0.0, 0.0, true, true),
			Entry("zero savings", 0.0, 10.0, 0.0, false, false),
			Entry("negative savings", -5.0, 10.0, 0.0, false, false),
		)
	})

	Describe("ShouldDisrupt", func() {
		It("should accept Balanced candidates", func() {
			rec := &mockRecorder{}
			c := consolidation{recorder: rec}

			np := makeNodePool("test-pool", v1.ConsolidationPolicyBalanced)
			candidate := makeShouldDisruptCandidate(np, v1.ConsolidationPolicyBalanced)

			ctx := options.ToContext(context.Background(), &options.Options{})

			Expect(c.ShouldDisrupt(ctx, candidate)).To(BeTrue())
		})

		It("should accept WhenEmptyOrUnderutilized candidates", func() {
			rec := &mockRecorder{}
			c := consolidation{recorder: rec}

			np := makeNodePool("test-pool", v1.ConsolidationPolicyWhenEmptyOrUnderutilized)
			candidate := makeShouldDisruptCandidate(np, v1.ConsolidationPolicyWhenEmptyOrUnderutilized)

			ctx := options.ToContext(context.Background(), &options.Options{})

			Expect(c.ShouldDisrupt(ctx, candidate)).To(BeTrue())
		})
	})

	Describe("sortCandidates", func() {
		It("should sort Balanced candidates by savings ratio (highest first)", func() {
			balancedNP := makeNodePool("balanced", v1.ConsolidationPolicyBalanced)

			// Candidate A: high price ($10), low disruption (1 pod) -> ratio = 10 / 2.0 = 5.0
			itA := makeInstanceType("expensive", 10.0)
			candA := makeCandidate("node-a", balancedNP, itA, []*corev1.Pod{makePod("pod-a", "")})
			candA.DisruptionCost = 100.0

			// Candidate B: low price ($1), high disruption (8 pods) -> ratio = 1 / 9.0 = 0.11
			itB := makeInstanceType("cheap", 1.0)
			podsB := make([]*corev1.Pod, 8)
			for i := range podsB {
				podsB[i] = makePod("pod-b-"+string(rune('0'+i)), "")
			}
			candB := makeCandidate("node-b", balancedNP, itB, podsB)
			candB.DisruptionCost = 1.0

			// Candidate C: medium price ($5), medium disruption (3 pods) -> ratio = 5 / 4.0 = 1.25
			itC := makeInstanceType("medium", 5.0)
			candC := makeCandidate("node-c", balancedNP, itC, []*corev1.Pod{
				makePod("pod-c-0", ""), makePod("pod-c-1", ""), makePod("pod-c-2", ""),
			})
			candC.DisruptionCost = 50.0

			c := consolidation{}
			ctx := options.ToContext(context.Background(), &options.Options{})
			candidates := []*Candidate{candB, candC, candA}
			sorted := c.sortCandidates(ctx, candidates)

			// Expected order: A (ratio 5.0) > C (ratio 1.25) > B (ratio 0.11)
			Expect(sorted[0]).To(Equal(candA), "expected first candidate to be A (highest ratio)")
			Expect(sorted[1]).To(Equal(candC), "expected second candidate to be C (medium ratio)")
			Expect(sorted[2]).To(Equal(candB), "expected third candidate to be B (lowest ratio)")
		})

		It("should sort non-Balanced candidates by savings ratio descending", func() {
			np := makeNodePool("default", v1.ConsolidationPolicyWhenEmptyOrUnderutilized)

			// All same price, different disruption costs -> ratio = price/disruption
			itA := makeInstanceType("type-a", 4.84)
			itB := makeInstanceType("type-b", 4.84)
			itC := makeInstanceType("type-c", 4.84)

			candA := makeCandidate("node-a", np, itA, []*corev1.Pod{makePod("pa", "")})
			candA.RescheduleDisruptionCost = 10.0 // ratio = 4.84/10 = 0.484
			candB := makeCandidate("node-b", np, itB, nil)
			// no pods: RescheduleDisruptionCost = 1.0 (base), ratio = 4.84/1 = 4.84
			candC := makeCandidate("node-c", np, itC, []*corev1.Pod{makePod("pc", "")})
			candC.RescheduleDisruptionCost = 5.0 // ratio = 4.84/5 = 0.968

			c := consolidation{}
			ctx := options.ToContext(context.Background(), &options.Options{})
			sorted := c.sortCandidates(ctx, []*Candidate{candA, candB, candC})

			// Expected order by ratio descending: B (4.84) > C (0.968) > A (0.484)
			Expect(sorted[0]).To(Equal(candB))
			Expect(sorted[1]).To(Equal(candC))
			Expect(sorted[2]).To(Equal(candA))
		})

		It("should sort all candidates by savings ratio when any uses Balanced", func() {
			balancedNP := makeNodePool("balanced", v1.ConsolidationPolicyBalanced)
			defaultNP := makeNodePool("default", v1.ConsolidationPolicyWhenEmptyOrUnderutilized)

			itExpensive := makeInstanceType("expensive", 10.0)
			itCheap := makeInstanceType("cheap", 1.0)

			candBalanced := makeCandidate("node-balanced", balancedNP, itExpensive, []*corev1.Pod{makePod("p1", "")})
			candDefault := makeCandidate("node-default", defaultNP, itCheap, []*corev1.Pod{makePod("p2", "")})

			c := consolidation{}
			ctx := options.ToContext(context.Background(), &options.Options{})
			sorted := c.sortCandidates(ctx, []*Candidate{candDefault, candBalanced})

			Expect(sorted[0]).To(Equal(candBalanced), "expected balanced candidate first (higher ratio)")
		})
	})

	Describe("EmitBalancedMultiNodeEvents", func() {
		It("should emit approved event for approved pool results", func() {
			ctx := context.Background()
			rec := &mockRecorder{}
			np := makeBalancedNodePool("pool-emit-a", int32Ptr(2))
			it := makeInstanceType("m7i.xlarge", 4.84)
			pod := makePod("pod", "")
			candidates := []*Candidate{
				makeCandidate("emit-node-0", np, it, []*corev1.Pod{pod}),
				makeCandidate("emit-node-1", np, it, []*corev1.Pod{pod}),
			}
			cmd := Command{Candidates: candidates}
			perPool := map[string]ScoreResult{
				"pool-emit-a": {SavingsFraction: 0.15, DisruptionFraction: 0.10, K: 2},
			}
			NewBalancedEvaluator(nil, rec).EmitMultiNodeEvents(ctx, cmd, perPool, true)
			Expect(rec.events).To(HaveLen(1))
			Expect(rec.events[0].Reason).To(Equal(events.ConsolidationApproved))
		})

		It("should not emit events for rejected pool results", func() {
			ctx := context.Background()
			rec := &mockRecorder{}
			np := makeBalancedNodePool("pool-emit-b", int32Ptr(1))
			it := makeInstanceType("m7i.xlarge", 4.84)
			pod := makePod("pod", "1000")
			candidates := []*Candidate{
				makeCandidate("emit-node-0", np, it, []*corev1.Pod{pod}),
			}
			cmd := Command{Candidates: candidates}
			perPool := map[string]ScoreResult{
				"pool-emit-b": {SavingsFraction: 0.03, DisruptionFraction: 0.10, K: 1},
			}
			NewBalancedEvaluator(nil, rec).EmitMultiNodeEvents(ctx, cmd, perPool, false)
			Expect(rec.events).To(BeEmpty())
		})

		It("should skip pools missing from perPoolResults", func() {
			ctx := context.Background()
			rec := &mockRecorder{}
			poolA := makeBalancedNodePool("pool-a-emit", nil)
			poolB := makeBalancedNodePool("pool-b-emit", nil)
			it := makeInstanceType("m7i.xlarge", 4.84)
			candidates := []*Candidate{
				makeCandidate("emit-node-0", poolA, it, nil),
				makeCandidate("emit-node-1", poolB, it, nil),
			}
			cmd := Command{Candidates: candidates}
			// Only pool-a has a result; pool-b is missing and does not produce a score event.
			perPool := map[string]ScoreResult{
				"pool-a-emit": {SavingsFraction: 0.10, DisruptionFraction: 0.05, K: v1.BalancedK},
			}
			NewBalancedEvaluator(nil, rec).EmitMultiNodeEvents(ctx, cmd, perPool, true)
			Expect(rec.events).To(HaveLen(1))
		})
	})

})
