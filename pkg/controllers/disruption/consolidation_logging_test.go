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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	pkgscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

const (
	// floatComparisonDelta is the tolerance for floating point comparisons in tests
	floatComparisonDelta = 0.001
)

var _ = Describe("getValidationFailureReason", func() {
	It("maps validation errors to metric reasons", func() {
		tests := []struct {
			name     string
			err      error
			expected string
		}{
			{
				name:     "nil error",
				err:      nil,
				expected: "unknown",
			},
			{
				name:     "non-validation error",
				err:      errors.New("some other error"),
				expected: "unknown",
			},
			{
				name:     "budget error - nominated",
				err:      NewBudgetValidationError(errors.New("a candidate was nominated during validation")),
				expected: "budget",
			},
			{
				name:     "budget error - budget",
				err:      NewBudgetValidationError(errors.New("can no longer be disrupted without violating budgets")),
				expected: "budget",
			},
			{
				name:     "scheduling error",
				err:      NewSchedulingValidationError(errors.New("scheduling simulation produced new results")),
				expected: "scheduling",
			},
			{
				name:     "churn error - no longer valid",
				err:      NewChurnValidationError(errors.New("candidates are no longer valid")),
				expected: "churn",
			},
			{
				name:     "churn error - churn",
				err:      NewChurnValidationError(errors.New("pod churn detected")),
				expected: "churn",
			},
			{
				name:     "generic validation error",
				err:      NewValidationError(errors.New("something unexpected")),
				expected: "unknown",
			},
		}

		for _, tt := range tests {
			By(tt.name)
			result := getValidationFailureReason(tt.err)
			Expect(result).To(Equal(tt.expected), tt.name)
		}
	})
})

var _ = Describe("Command.EstimatedSavings", func() {
	It("sums the cost of multiple replacement NodeClaims", func() {
		// This test verifies that EstimatedSavings correctly sums costs from multiple
		// replacement NodeClaims. While current consolidation is 1->1, this future-proofs
		// for potential N->M consolidation scenarios.

		// Create mock candidates with pricing
		candidate1 := mockCandidate("node-1")
		candidate1.instanceType = &cloudprovider.InstanceType{
			Name: "source-type",
			Offerings: cloudprovider.Offerings{
				{Price: 0.50, Available: true},
			},
		}
		candidate1.Price = 0.50

		cmd := Command{
			Candidates:   []*Candidate{candidate1},
			Replacements: []*Replacement{{}, {}}, // 2 replacements
			Results: scheduling.Results{
				NewNodeClaims: []*scheduling.NodeClaim{
					{
						NodeClaimTemplate: scheduling.NodeClaimTemplate{
							InstanceTypeOptions: []*cloudprovider.InstanceType{
								{
									Name: "dest-type-1",
									Offerings: cloudprovider.Offerings{
										{Price: 0.20, Available: true},
									},
								},
							},
						},
					},
					{
						NodeClaimTemplate: scheduling.NodeClaimTemplate{
							InstanceTypeOptions: []*cloudprovider.InstanceType{
								{
									Name: "dest-type-2",
									Offerings: cloudprovider.Offerings{
										{Price: 0.15, Available: true},
									},
								},
							},
						},
					},
				},
			},
		}

		// Expected: sourcePrice (0.50) - (destPrice1 (0.20) + destPrice2 (0.15)) = 0.15
		savings := cmd.EstimatedSavings()
		expectedSavings := 0.15

		// Use tolerance for floating point comparison
		Expect(savings).To(BeNumerically("~", expectedSavings, floatComparisonDelta), "verifying multi-NodeClaim cost summing")
	})

	It("only considers offerings valid for an on-demand-only NodePool", func() {
		// This test verifies that EstimatedSavings only considers offerings that are valid for the NodePool under
		// consideration (e.g. on-demand-only NodePool)
		candidate := mockCandidate("node-1")
		candidate.instanceType = fake.NewInstanceType("source-type",
			fake.WithOfferings(cloudprovider.Offering{
				Available: true,
				Price:     0.50,
				Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone: "us-west-2a",
				}),
			}),
		)
		candidate.Price = 0.50

		destType := fake.NewInstanceType("dest-type",
			fake.WithOfferings(
				cloudprovider.Offering{
					Available: true,
					Price:     0.10,
					Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:  v1.CapacityTypeSpot,
						corev1.LabelTopologyZone: "us-west-2a",
					}),
				},
				cloudprovider.Offering{
					Available: true,
					Price:     0.40,
					Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
						corev1.LabelTopologyZone: "us-west-2a",
					}),
				},
			),
		)

		nodeClaimReqs := pkgscheduling.NewRequirements(
			pkgscheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
		)

		cmd := Command{
			Candidates:   []*Candidate{candidate},
			Replacements: []*Replacement{{}},
			Results: scheduling.Results{
				NewNodeClaims: []*scheduling.NodeClaim{
					{
						NodeClaimTemplate: scheduling.NodeClaimTemplate{
							InstanceTypeOptions: []*cloudprovider.InstanceType{destType},
							Requirements:        nodeClaimReqs,
						},
					},
				},
			},
		}

		// Expected: sourcePrice (0.50) - destPrice (0.40, the on-demand offering) = 0.10
		// Without filtering: 0.50 - 0.10 = 0.40 (bug: spot price used as dest price)
		savings := cmd.EstimatedSavings()
		expectedSavings := 0.10

		Expect(savings).To(BeNumerically("~", expectedSavings, floatComparisonDelta))
	})

	It("applies non-capacity-type requirements such as zone", func() {
		// Verifies EstimatedSavings filters AZ requirements properly
		candidate := mockCandidate("node-1")
		candidate.instanceType = fake.NewInstanceType("source-type",
			fake.WithOfferings(cloudprovider.Offering{
				Available: true,
				Price:     0.50,
				Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone: "us-west-2a",
				}),
			}),
		)
		candidate.Price = 0.50

		destType := fake.NewInstanceType("dest-type",
			fake.WithOfferings(
				cloudprovider.Offering{
					Available: true,
					Price:     0.05,
					Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
						corev1.LabelTopologyZone: "us-west-2b",
					}),
				},
				cloudprovider.Offering{
					Available: true,
					Price:     0.45,
					Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
						corev1.LabelTopologyZone: "us-west-2a",
					}),
				},
			),
		)

		nodeClaimReqs := pkgscheduling.NewRequirements(
			pkgscheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
		)

		cmd := Command{
			Candidates:   []*Candidate{candidate},
			Replacements: []*Replacement{{}},
			Results: scheduling.Results{
				NewNodeClaims: []*scheduling.NodeClaim{
					{
						NodeClaimTemplate: scheduling.NodeClaimTemplate{
							InstanceTypeOptions: []*cloudprovider.InstanceType{destType},
							Requirements:        nodeClaimReqs,
						},
					},
				},
			},
		}

		// Expected: sourcePrice (0.50) - destPrice (0.45, us-west-2a) = 0.05
		// The us-west-2b offering (0.05) must be excluded because the NodePool requires us-west-2a.
		savings := cmd.EstimatedSavings()
		expectedSavings := 0.05

		Expect(savings).To(BeNumerically("~", expectedSavings, floatComparisonDelta), "compatible filter must apply to non-capacity-type requirements")
	})

	It("keeps every legal offering when capacity type is unrestricted", func() {
		// Negative control such that we consider all offerings for NodePool with no capacity-type restriction
		candidate := mockCandidate("node-1")
		candidate.instanceType = fake.NewInstanceType("source-type",
			fake.WithOfferings(cloudprovider.Offering{
				Available: true,
				Price:     0.50,
				Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone: "us-west-2a",
				}),
			}),
		)
		candidate.Price = 0.50

		destType := fake.NewInstanceType("dest-type",
			fake.WithOfferings(
				cloudprovider.Offering{
					Available: true,
					Price:     0.10,
					Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:  v1.CapacityTypeSpot,
						corev1.LabelTopologyZone: "us-west-2a",
					}),
				},
				cloudprovider.Offering{
					Available: true,
					Price:     0.40,
					Requirements: pkgscheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
						corev1.LabelTopologyZone: "us-west-2a",
					}),
				},
			),
		)

		nodeClaimReqs := pkgscheduling.NewRequirements(
			pkgscheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand, v1.CapacityTypeSpot),
		)

		cmd := Command{
			Candidates:   []*Candidate{candidate},
			Replacements: []*Replacement{{}},
			Results: scheduling.Results{
				NewNodeClaims: []*scheduling.NodeClaim{
					{
						NodeClaimTemplate: scheduling.NodeClaimTemplate{
							InstanceTypeOptions: []*cloudprovider.InstanceType{destType},
							Requirements:        nodeClaimReqs,
						},
					},
				},
			},
		}

		// Expected: sourcePrice (0.50) - destPrice (0.10, spot is legal) = 0.40
		savings := cmd.EstimatedSavings()
		expectedSavings := 0.40

		Expect(savings).To(BeNumerically("~", expectedSavings, floatComparisonDelta), "fix must not exclude legal offerings")
	})

	It("handles edge cases", func() {
		tests := []struct {
			name            string
			cmd             Command
			expectedSavings float64
		}{
			{
				name: "empty NodeClaim list",
				cmd: Command{
					Replacements: []*Replacement{{}},
				},
				expectedSavings: 0.0,
			},
			{
				name: "NodeClaim with no InstanceTypeOptions",
				cmd: Command{
					Replacements: []*Replacement{{}},
					Results: scheduling.Results{
						NewNodeClaims: []*scheduling.NodeClaim{
							{
								NodeClaimTemplate: scheduling.NodeClaimTemplate{
									InstanceTypeOptions: []*cloudprovider.InstanceType{},
								},
							},
						},
					},
				},
				expectedSavings: 0.0,
			},
			{
				name: "NodeClaim with empty Offerings",
				cmd: Command{
					Replacements: []*Replacement{{}},
					Results: scheduling.Results{
						NewNodeClaims: []*scheduling.NodeClaim{
							{
								NodeClaimTemplate: scheduling.NodeClaimTemplate{
									InstanceTypeOptions: []*cloudprovider.InstanceType{
										{
											Name:      "instance-type",
											Offerings: cloudprovider.Offerings{},
										},
									},
								},
							},
						},
					},
				},
				expectedSavings: 0.0,
			},
			{
				name: "delete consolidation - no replacements",
				cmd: Command{
					Replacements: []*Replacement{},
				},
				expectedSavings: 0.0, // No candidates, so sourcePrice = 0
			},
		}

		for _, tt := range tests {
			By(tt.name)
			savings := tt.cmd.EstimatedSavings()
			Expect(savings).To(BeNumerically("~", tt.expectedSavings, floatComparisonDelta), tt.name)
		}
	})
})

var _ = Describe("Command.String", func() {
	It("formats commands", func() {
		tests := []struct {
			name     string
			cmd      Command
			expected string
		}{
			{
				name: "delete single node",
				cmd: Command{
					Candidates:   []*Candidate{mockCandidate("node-1")},
					Replacements: []*Replacement{},
				},
				expected: "delete: [node-1]",
			},
			{
				name: "delete multiple nodes",
				cmd: Command{
					Candidates:   []*Candidate{mockCandidate("node-1"), mockCandidate("node-2")},
					Replacements: []*Replacement{},
				},
				expected: "delete: [node-1, node-2]",
			},
			{
				name: "replace with single replacement",
				cmd: Command{
					Candidates:   []*Candidate{mockCandidate("node-1")},
					Replacements: []*Replacement{{}},
				},
				expected: "replace: [node-1] -> [1 replacement]",
			},
			{
				name: "replace with multiple replacements",
				cmd: Command{
					Candidates:   []*Candidate{mockCandidate("node-1")},
					Replacements: []*Replacement{{}, {}},
				},
				expected: "replace: [node-1] -> [2 replacements]",
			},
			{
				name: "empty command",
				cmd: Command{
					Candidates:   []*Candidate{},
					Replacements: []*Replacement{},
				},
				expected: "no-op: []",
			},
		}

		for _, tt := range tests {
			By(tt.name)
			result := tt.cmd.String()
			Expect(result).To(Equal(tt.expected), tt.name)
		}
	})
})

func mockCandidate(name string) *Candidate {
	return &Candidate{
		StateNode: &state.StateNode{
			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
			},
		},
	}
}

var _ = Describe("Command.LogValues", func() {
	It("sums pod-count across candidates", func() {
		// Create candidates with different numbers of reschedulable pods
		c1 := mockCandidate("node-1")
		c1.NodeClaim = &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}}
		c1.reschedulablePods = []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-2"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-3"}},
		}

		c2 := mockCandidate("node-2")
		c2.NodeClaim = &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-2"}}
		c2.reschedulablePods = []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-4"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-5"}},
		}

		cmd := Command{
			Candidates:   []*Candidate{c1, c2},
			Replacements: []*Replacement{},
		}

		logValues := cmd.LogValues()
		// LogValues returns key-value pairs: find the "pod-count" key and check its value
		var podCount int
		for i := 0; i < len(logValues)-1; i += 2 {
			if logValues[i] == "pod-count" {
				podCount = logValues[i+1].(int)
				break
			}
		}

		// pod-count should be 3 + 2 = 5, not just 2 (the last candidate's count)
		Expect(podCount).To(Equal(5), "sum of all candidates' pods")
	})
})

var _ = Describe("ConsolidationCandidate event", func() {
	It("publishes one event for the Node and one for the NodeClaim", func() {
		recorder := test.NewEventRecorder()

		// Create a mock candidate with Node and NodeClaim
		candidate := &Candidate{
			StateNode: &state.StateNode{
				Node: &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-node",
						UID:  "test-node-uid",
					},
				},
				NodeClaim: &v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-nodeclaim",
						UID:  "test-nodeclaim-uid",
					},
				},
			},
		}

		// Create a command with candidates but no replacements (delete consolidation)
		cmd := Command{
			Candidates:   []*Candidate{candidate},
			Replacements: []*Replacement{}, // Empty for delete consolidation
		}

		// Publish the event
		recorder.Publish(disruptionevents.ConsolidationCandidate(
			candidate.Node,
			candidate.NodeClaim,
			cmd.String(),
			0.0, // Use fixed savings to avoid EstimatedSavings calculation
		)...)

		// Verify the event was published
		Expect(recorder.Calls(events.ConsolidationCandidate)).To(Equal(2))
	})
})

var _ = Describe("launchCapacityType", func() {
	const onDemandAlt = "on-demand-alt"
	BeforeEach(func() {
		Expect(cloudprovider.SetCapacityTypeClassResolver(cloudprovider.DefaultCapacityTypeClasses.With(cloudprovider.CapacityTypeClasses{
			onDemandAlt: cloudprovider.CapacityTypeClassOnDemand,
		}))).To(Succeed())
		DeferCleanup(test.ResetCapacityTypeClasses)
	})
	DescribeTable("picks the launch capacity type",
		func(allowed []string, expected string) {
			requirement := pkgscheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, allowed...)
			Expect(launchCapacityType(requirement)).To(Equal(expected))
		},
		Entry("reserved first", []string{v1.CapacityTypeReserved, v1.CapacityTypeSpot, v1.CapacityTypeOnDemand}, v1.CapacityTypeReserved),
		Entry("spot before on-demand", []string{v1.CapacityTypeSpot, v1.CapacityTypeOnDemand}, v1.CapacityTypeSpot),
		Entry("on-demand only", []string{v1.CapacityTypeOnDemand}, v1.CapacityTypeOnDemand),
		Entry("an additional on-demand class capacity type", []string{onDemandAlt}, onDemandAlt),
		Entry("more than one on-demand class capacity type", []string{v1.CapacityTypeOnDemand, onDemandAlt}, string(cloudprovider.CapacityTypeClassOnDemand)),
		Entry("spot before an additional on-demand class capacity type", []string{v1.CapacityTypeSpot, onDemandAlt}, v1.CapacityTypeSpot),
	)
})
