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

package health

import (
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var _ = Describe("Repair Policies", func() {
	supportedActions := sets.New(cloudprovider.RebootNode, cloudprovider.ReplaceNode)
	defaultFallback := cloudprovider.RepairPolicy{
		ConditionType:      "AcceleratorReady",
		ConditionStatus:    corev1.ConditionFalse,
		TolerationDuration: 30 * time.Minute,
		Action:             cloudprovider.ReplaceNode,
	}
	validSpecific := cloudprovider.RepairPolicy{
		ConditionType:      "AcceleratorReady",
		ConditionStatus:    corev1.ConditionFalse,
		ReasonRegex:        `^NvidiaXID(48|63|95)Error$`,
		TolerationDuration: 10 * time.Minute,
		Action:             cloudprovider.RebootNode,
	}
	negativeDuration := -time.Second

	DescribeTable("validating complete policy sets",
		func(policies []cloudprovider.RepairPolicy, actions sets.Set[cloudprovider.RepairAction], errorSubstring string) {
			_, err := NewRepairPolicyMatcher(policies, actions)
			if errorSubstring == "" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ContainSubstring(errorSubstring)))
			}
		},
		Entry("accepts valid policies",
			[]cloudprovider.RepairPolicy{validSpecific, defaultFallback},
			supportedActions,
			"",
		),
		Entry("accepts every Kubernetes condition status",
			[]cloudprovider.RepairPolicy{
				{
					ConditionType:   "ConditionTrue",
					ConditionStatus: corev1.ConditionTrue,
					Action:          cloudprovider.ReplaceNode,
				},
				{
					ConditionType:   "ConditionFalse",
					ConditionStatus: corev1.ConditionFalse,
					ReasonRegex:     ".*",
					Action:          cloudprovider.ReplaceNode,
				},
				{
					ConditionType:   "ConditionUnknown",
					ConditionStatus: corev1.ConditionUnknown,
					ReasonRegex:     ".*",
					Action:          cloudprovider.ReplaceNode,
				},
			},
			supportedActions,
			"",
		),
		Entry("rejects an empty condition type",
			[]cloudprovider.RepairPolicy{{
				ConditionStatus: corev1.ConditionFalse,
				Action:          cloudprovider.ReplaceNode,
			}},
			supportedActions,
			"empty condition type",
		),
		Entry("rejects an invalid condition status",
			[]cloudprovider.RepairPolicy{{
				ConditionType: "AcceleratorReady",
				Action:        cloudprovider.ReplaceNode,
			}},
			supportedActions,
			"invalid condition status",
		),
		Entry("rejects an invalid reason regex",
			[]cloudprovider.RepairPolicy{
				{
					ConditionType:   "AcceleratorReady",
					ConditionStatus: corev1.ConditionFalse,
					ReasonRegex:     "[",
					Action:          cloudprovider.RebootNode,
				},
				defaultFallback,
			},
			supportedActions,
			"invalid reason regex",
		),
		Entry("rejects an unsupported action",
			[]cloudprovider.RepairPolicy{validSpecific, defaultFallback},
			sets.New(cloudprovider.ReplaceNode),
			`unsupported action "RebootNode"`,
		),
		Entry("rejects a negative toleration",
			[]cloudprovider.RepairPolicy{{
				ConditionType:      "AcceleratorReady",
				ConditionStatus:    corev1.ConditionFalse,
				TolerationDuration: -time.Second,
				Action:             cloudprovider.ReplaceNode,
			}},
			supportedActions,
			"negative toleration duration",
		),
		Entry("rejects a negative termination grace period",
			[]cloudprovider.RepairPolicy{{
				ConditionType:          "AcceleratorReady",
				ConditionStatus:        corev1.ConditionFalse,
				TerminationGracePeriod: &negativeDuration,
				Action:                 cloudprovider.ReplaceNode,
			}},
			supportedActions,
			"negative termination grace period",
		),
		Entry("rejects a negative priority",
			[]cloudprovider.RepairPolicy{{
				ConditionType:   "AcceleratorReady",
				ConditionStatus: corev1.ConditionFalse,
				Priority:        -1,
				Action:          cloudprovider.ReplaceNode,
			}},
			supportedActions,
			"priority -1 outside the supported range [0, 100]",
		),
		Entry("rejects a priority above the supported range",
			[]cloudprovider.RepairPolicy{{
				ConditionType:   "AcceleratorReady",
				ConditionStatus: corev1.ConditionFalse,
				Priority:        101,
				Action:          cloudprovider.ReplaceNode,
			}},
			supportedActions,
			"priority 101 outside the supported range [0, 100]",
		),
		Entry("rejects a missing default fallback",
			[]cloudprovider.RepairPolicy{validSpecific},
			supportedActions,
			"must define one default fallback",
		),
		Entry("rejects multiple default fallbacks",
			[]cloudprovider.RepairPolicy{
				defaultFallback,
				{
					ConditionType:   "StorageReady",
					ConditionStatus: corev1.ConditionFalse,
					Action:          cloudprovider.ReplaceNode,
				},
			},
			supportedActions,
			"multiple default fallbacks",
		),
		Entry("requires a replacement default fallback",
			[]cloudprovider.RepairPolicy{{
				ConditionType:   "AcceleratorReady",
				ConditionStatus: corev1.ConditionFalse,
				Action:          cloudprovider.RebootNode,
			}},
			supportedActions,
			"default fallback policy",
		),
	)

	It("includes the complete policy in validation errors", func() {
		policies := []cloudprovider.RepairPolicy{
			{
				ConditionType:   "AcceleratorReady",
				ConditionStatus: corev1.ConditionFalse,
				ReasonRegex:     "[",
				Priority:        17,
				Action:          cloudprovider.RebootNode,
			},
			defaultFallback,
		}

		_, err := NewRepairPolicyMatcher(policies, supportedActions)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("ConditionType:AcceleratorReady"))
		Expect(err.Error()).To(ContainSubstring("ReasonRegex:["))
		Expect(err.Error()).To(ContainSubstring("Priority:17"))
	})

	Context("Matching", func() {
		var now time.Time
		var condition corev1.NodeCondition
		var policies []cloudprovider.RepairPolicy
		var matcher *RepairPolicyMatcher
		evaluate := func(matcher *RepairPolicyMatcher, condition corev1.NodeCondition, now time.Time) *RepairPolicyEvaluation {
			evaluation, ok := matcher.evaluateCondition(condition, now)
			if !ok {
				return nil
			}
			return &evaluation
		}

		BeforeEach(func() {
			now = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
			condition = corev1.NodeCondition{
				Type:               "AcceleratorReady",
				Status:             corev1.ConditionFalse,
				Reason:             "NvidiaXID48Error",
				LastTransitionTime: metav1.NewTime(now),
			}
			policies = []cloudprovider.RepairPolicy{
				{
					ConditionType:      condition.Type,
					ConditionStatus:    condition.Status,
					ReasonRegex:        `XID(48|63)`,
					TolerationDuration: 10 * time.Minute,
					Action:             cloudprovider.RebootNode,
				},
				{
					ConditionType:      condition.Type,
					ConditionStatus:    condition.Status,
					ReasonRegex:        `48Error$`,
					TolerationDuration: 30 * time.Minute,
					Action:             cloudprovider.ReplaceNode,
				},
				{
					ConditionType:   condition.Type,
					ConditionStatus: condition.Status,
					Action:          cloudprovider.ReplaceNode,
				},
			}
			var err error
			matcher, err = NewRepairPolicyMatcher(policies, supportedActions)
			Expect(err).NotTo(HaveOccurred())
		})

		It("suppresses an eligible fallback while specific policies are waiting", func() {
			decision := evaluate(matcher, condition, now.Add(5*time.Minute))
			Expect(decision).NotTo(BeNil())
			Expect(decision.Fallback).To(BeFalse())
			Expect(decision.MatchingPolicies).To(Equal(2))
			Expect(decision.EligiblePolicies).To(BeZero())
			Expect(decision.Action).To(Equal(cloudprovider.RebootNode))
			Expect(decision.EligibleAt).To(Equal(now.Add(10 * time.Minute)))
		})

		It("merges independently eligible specific policies by action", func() {
			decision := evaluate(matcher, condition, now.Add(15*time.Minute))
			Expect(decision).NotTo(BeNil())
			Expect(decision.Action).To(Equal(cloudprovider.RebootNode))
			Expect(decision.EligiblePolicies).To(Equal(1))
			Expect(decision.EligibleAt).To(Equal(now.Add(10 * time.Minute)))

			decision = evaluate(matcher, condition, now.Add(35*time.Minute))
			Expect(decision.Action).To(Equal(cloudprovider.ReplaceNode))
			Expect(decision.EligiblePolicies).To(Equal(2))
			Expect(decision.EligibleAt).To(Equal(now.Add(30 * time.Minute)))
		})

		It("returns scoring inputs only for reason-matching policies whose toleration has elapsed", func() {
			result := evaluate(matcher, condition, now.Add(15*time.Minute))
			Expect(result.eligiblePolicies).To(Equal([]eligibleRepairPolicy{{
				Priority:   0,
				EligibleAt: now.Add(10 * time.Minute),
			}}))

			result = evaluate(matcher, condition, now.Add(35*time.Minute))
			Expect(result.EligiblePolicies).To(Equal(2))
		})

		It("exposes complete per-condition evaluations", func() {
			waiting := evaluate(matcher, condition, now.Add(5*time.Minute))
			Expect(waiting).NotTo(BeNil())
			Expect(waiting.EligiblePolicies).To(BeZero())
			Expect(waiting.EligibleAt).To(Equal(now.Add(10 * time.Minute)))
			Expect(evaluate(matcher, condition, now.Add(15*time.Minute))).To(Equal(&RepairPolicyEvaluation{
				ConditionType:    condition.Type,
				ConditionStatus:  condition.Status,
				Reason:           condition.Reason,
				Action:           cloudprovider.RebootNode,
				EligibleAt:       now.Add(10 * time.Minute),
				MatchingPolicies: 2,
				EligiblePolicies: 1,
				eligiblePolicies: []eligibleRepairPolicy{{
					Priority:   0,
					EligibleAt: now.Add(10 * time.Minute),
				}},
			}))
		})

		It("retains the earliest eligibility for the selected action", func() {
			policies = []cloudprovider.RepairPolicy{
				{
					ConditionType:      condition.Type,
					ConditionStatus:    condition.Status,
					ReasonRegex:        "XID48",
					TolerationDuration: 20 * time.Minute,
					Action:             cloudprovider.ReplaceNode,
				},
				{
					ConditionType:      condition.Type,
					ConditionStatus:    condition.Status,
					ReasonRegex:        "48Error",
					TolerationDuration: 10 * time.Minute,
					Action:             cloudprovider.ReplaceNode,
				},
				{
					ConditionType:   condition.Type,
					ConditionStatus: condition.Status,
					Action:          cloudprovider.ReplaceNode,
				},
			}
			reversed := slices.Clone(policies)
			slices.Reverse(reversed)
			for _, orderedPolicies := range [][]cloudprovider.RepairPolicy{policies, reversed} {
				sameActionMatcher, err := NewRepairPolicyMatcher(orderedPolicies, supportedActions)
				Expect(err).NotTo(HaveOccurred())

				decision := evaluate(sameActionMatcher, condition, now.Add(25*time.Minute))
				Expect(decision).NotTo(BeNil())
				Expect(decision.EligiblePolicies).To(Equal(2))
				Expect(decision.EligibleAt).To(Equal(now.Add(10 * time.Minute)))
			}
		})

		It("selects the shortest termination grace period from eligible policies", func() {
			longGracePeriod := 15 * time.Minute
			shortGracePeriod := 5 * time.Minute
			gracePeriodMatcher, err := NewRepairPolicyMatcher([]cloudprovider.RepairPolicy{
				{
					ConditionType:          condition.Type,
					ConditionStatus:        condition.Status,
					ReasonRegex:            "XID48",
					TerminationGracePeriod: &longGracePeriod,
					Action:                 cloudprovider.ReplaceNode,
				},
				{
					ConditionType:          condition.Type,
					ConditionStatus:        condition.Status,
					ReasonRegex:            "48Error",
					TerminationGracePeriod: &shortGracePeriod,
					Action:                 cloudprovider.ReplaceNode,
				},
				{
					ConditionType:   condition.Type,
					ConditionStatus: condition.Status,
					Action:          cloudprovider.ReplaceNode,
				},
			}, supportedActions)
			Expect(err).NotTo(HaveOccurred())

			result := evaluate(gracePeriodMatcher, condition, now)
			Expect(result).NotTo(BeNil())
			Expect(result.TerminationGracePeriod).NotTo(BeNil())
			Expect(*result.TerminationGracePeriod).To(Equal(shortGracePeriod))
		})

		It("selects the shortest termination grace period across eligible actions", func() {
			longGracePeriod := 15 * time.Minute
			shortGracePeriod := 5 * time.Minute
			crossActionPolicies := []cloudprovider.RepairPolicy{
				{
					ConditionType:          condition.Type,
					ConditionStatus:        condition.Status,
					ReasonRegex:            "XID48",
					TerminationGracePeriod: &longGracePeriod,
					Action:                 cloudprovider.ReplaceNode,
				},
				{
					ConditionType:          condition.Type,
					ConditionStatus:        condition.Status,
					ReasonRegex:            "48Error",
					TerminationGracePeriod: &shortGracePeriod,
					Action:                 cloudprovider.RebootNode,
				},
				{
					ConditionType:   condition.Type,
					ConditionStatus: condition.Status,
					Action:          cloudprovider.ReplaceNode,
				},
			}
			reversed := slices.Clone(crossActionPolicies)
			slices.Reverse(reversed)
			for _, orderedPolicies := range [][]cloudprovider.RepairPolicy{crossActionPolicies, reversed} {
				gracePeriodMatcher, err := NewRepairPolicyMatcher(orderedPolicies, supportedActions)
				Expect(err).NotTo(HaveOccurred())

				result := evaluate(gracePeriodMatcher, condition, now)
				Expect(result).NotTo(BeNil())
				Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
				Expect(result.TerminationGracePeriod).NotTo(BeNil())
				Expect(*result.TerminationGracePeriod).To(Equal(shortGracePeriod))
			}
		})

		It("uses the fallback for an unknown reason", func() {
			condition.Reason = "NewFailureCode"
			decision := evaluate(matcher, condition, now)
			Expect(decision).NotTo(BeNil())
			Expect(decision.EligiblePolicies).To(Equal(1))
			Expect(decision.Fallback).To(BeTrue())
			Expect(decision.Action).To(Equal(cloudprovider.ReplaceNode))
			Expect(decision.MatchingPolicies).To(Equal(1))
		})

		It("uses the default fallback across supported conditions", func() {
			crossConditionMatcher, err := NewRepairPolicyMatcher([]cloudprovider.RepairPolicy{
				defaultFallback,
				{
					ConditionType:   "StorageReady",
					ConditionStatus: corev1.ConditionFalse,
					ReasonRegex:     "^KnownStorageFailure$",
					Action:          cloudprovider.RebootNode,
				},
			}, supportedActions)
			Expect(err).NotTo(HaveOccurred())

			storageCondition := condition
			storageCondition.Type = "StorageReady"
			storageCondition.Reason = "NewStorageFailure"
			decision := evaluate(crossConditionMatcher, storageCondition, now.Add(31*time.Minute))

			Expect(decision).NotTo(BeNil())
			Expect(decision.Fallback).To(BeTrue())
			Expect(decision.Action).To(Equal(cloudprovider.ReplaceNode))
			Expect(decision.ConditionType).To(Equal(corev1.NodeConditionType("StorageReady")))
			Expect(decision.ConditionStatus).To(Equal(storageCondition.Status))
			Expect(decision.EligiblePolicies).To(Equal(1))
		})

		It("treats a non-empty match-all expression as a specific policy", func() {
			policies = []cloudprovider.RepairPolicy{
				{
					ConditionType:   condition.Type,
					ConditionStatus: condition.Status,
					ReasonRegex:     ".*",
					Action:          cloudprovider.RebootNode,
				},
				{
					ConditionType:   condition.Type,
					ConditionStatus: condition.Status,
					Action:          cloudprovider.ReplaceNode,
				},
			}
			matchAllMatcher, err := NewRepairPolicyMatcher(policies, supportedActions)
			Expect(err).NotTo(HaveOccurred())

			decision := evaluate(matchAllMatcher, condition, now)
			Expect(decision).NotTo(BeNil())
			Expect(decision.Fallback).To(BeFalse())
			Expect(decision.Action).To(Equal(cloudprovider.RebootNode))
		})

		It("does not match a different condition type or status", func() {
			differentType := condition
			differentType.Type = "DifferentCondition"
			Expect(evaluate(matcher, differentType, now)).To(BeNil())

			differentStatus := condition
			differentStatus.Status = corev1.ConditionTrue
			Expect(evaluate(matcher, differentStatus, now)).To(BeNil())
		})

		It("is independent of policy order after eligibility", func() {
			reversed := slices.Clone(policies)
			slices.Reverse(reversed)
			reversedMatcher, err := NewRepairPolicyMatcher(reversed, supportedActions)
			Expect(err).NotTo(HaveOccurred())
			Expect(evaluate(reversedMatcher, condition, now.Add(35*time.Minute))).To(
				Equal(evaluate(matcher, condition, now.Add(35*time.Minute))),
			)
		})

		It("uses action ordering to break waiting eligibility ties", func() {
			tiedPolicies := []cloudprovider.RepairPolicy{
				{
					ConditionType:      condition.Type,
					ConditionStatus:    condition.Status,
					ReasonRegex:        "XID48",
					TolerationDuration: 10 * time.Minute,
					Action:             cloudprovider.RebootNode,
				},
				{
					ConditionType:      condition.Type,
					ConditionStatus:    condition.Status,
					ReasonRegex:        "48Error",
					TolerationDuration: 10 * time.Minute,
					Action:             cloudprovider.ReplaceNode,
				},
				{
					ConditionType:   condition.Type,
					ConditionStatus: condition.Status,
					Action:          cloudprovider.ReplaceNode,
				},
			}
			forwardMatcher, err := NewRepairPolicyMatcher(tiedPolicies, supportedActions)
			Expect(err).NotTo(HaveOccurred())
			reversed := slices.Clone(tiedPolicies)
			slices.Reverse(reversed)
			reversedMatcher, err := NewRepairPolicyMatcher(reversed, supportedActions)
			Expect(err).NotTo(HaveOccurred())

			forwardDecision := evaluate(forwardMatcher, condition, now.Add(5*time.Minute))
			Expect(forwardDecision).NotTo(BeNil())
			Expect(forwardDecision.Action).To(Equal(cloudprovider.ReplaceNode))
			Expect(evaluate(reversedMatcher, condition, now.Add(5*time.Minute))).To(Equal(forwardDecision))
		})

		It("scores a node using its most urgent eligible policy", func() {
			nodeMatcher, err := NewRepairPolicyMatcher([]cloudprovider.RepairPolicy{
				{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
				{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
				{ConditionType: "Fallback", ConditionStatus: corev1.ConditionFalse, Action: cloudprovider.ReplaceNode},
			}, supportedActions)
			Expect(err).NotTo(HaveOccurred())
			node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{
					Type:               "LowPriority",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(now.Add(-180 * time.Minute)),
				},
				{
					Type:               "HighPriority",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(now.Add(-45 * time.Minute)),
				},
			}}}

			result := nodeMatcher.Evaluate(node, now)
			Expect(result.Score).To(Equal(float64(6)))
			Expect(result.Decision).NotTo(BeNil())
			Expect(result.Decision.ConditionType).To(Equal(corev1.NodeConditionType("HighPriority")))
		})

		It("uses earliest eligibility to break equal-priority condition ties", func() {
			nodeMatcher, err := NewRepairPolicyMatcher([]cloudprovider.RepairPolicy{
				{ConditionType: "Earlier", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 20 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
				{ConditionType: "Later", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 10 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
				{ConditionType: "Fallback", ConditionStatus: corev1.ConditionFalse, Action: cloudprovider.ReplaceNode},
			}, supportedActions)
			Expect(err).NotTo(HaveOccurred())
			earlier := corev1.NodeCondition{
				Type:               "Earlier",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(now.Add(-40 * time.Minute)),
			}
			later := corev1.NodeCondition{
				Type:               "Later",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(now.Add(-15 * time.Minute)),
			}

			for _, conditions := range [][]corev1.NodeCondition{{earlier, later}, {later, earlier}} {
				result := nodeMatcher.Evaluate(&corev1.Node{Status: corev1.NodeStatus{Conditions: conditions}}, now)
				Expect(result.Decision).NotTo(BeNil())
				Expect(result.Decision.ConditionType).To(Equal(corev1.NodeConditionType("Earlier")))
			}
		})

		It("reconstructs eligibility from the current condition after restart", func() {
			reasonPolicies := []cloudprovider.RepairPolicy{
				{
					ConditionType:      condition.Type,
					ConditionStatus:    condition.Status,
					ReasonRegex:        "^ImmediateReason$",
					TolerationDuration: 30 * time.Minute,
					Action:             cloudprovider.ReplaceNode,
				},
				{
					ConditionType:      condition.Type,
					ConditionStatus:    condition.Status,
					TolerationDuration: 2 * time.Hour,
					Action:             cloudprovider.ReplaceNode,
				},
			}
			condition.LastTransitionTime = metav1.NewTime(now.Add(-time.Hour))
			condition.Reason = "UnknownReason"
			initialMatcher, err := NewRepairPolicyMatcher(reasonPolicies, supportedActions)
			Expect(err).NotTo(HaveOccurred())
			initial := evaluate(initialMatcher, condition, now)
			Expect(initial).NotTo(BeNil())
			Expect(initial.EligiblePolicies).To(BeZero())

			condition.Reason = "ImmediateReason"
			restartedMatcher, err := NewRepairPolicyMatcher(reasonPolicies, supportedActions)
			Expect(err).NotTo(HaveOccurred())
			result := evaluate(restartedMatcher, condition, now)
			Expect(result).NotTo(BeNil())
			Expect(result.EligiblePolicies).To(Equal(1))
			Expect(result.EligibleAt).To(Equal(now.Add(-30 * time.Minute)))
		})
	})

})
