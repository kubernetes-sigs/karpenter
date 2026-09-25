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
		evaluate := func(matcher *RepairPolicyMatcher, now time.Time, conditions ...corev1.NodeCondition) RepairResult {
			return matcher.Evaluate(&corev1.Node{
				Status: corev1.NodeStatus{Conditions: conditions},
			}, now)
		}
		newMatcher := func(policies []cloudprovider.RepairPolicy) *RepairPolicyMatcher {
			policyMatcher, err := NewRepairPolicyMatcher(policies, supportedActions)
			Expect(err).NotTo(HaveOccurred())
			return policyMatcher
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
			matcher = newMatcher(policies)
		})

		It("suppresses an eligible fallback while specific policies are waiting", func() {
			Expect(evaluate(matcher, now.Add(5*time.Minute), condition).Action).To(BeEmpty())
			Expect(evaluate(matcher, now.Add(10*time.Minute), condition).Action).To(Equal(cloudprovider.RebootNode))
		})

		It("merges eligible policies into a single result", func() {
			result := evaluate(matcher, now.Add(15*time.Minute), condition)
			Expect(result.Action).To(Equal(cloudprovider.RebootNode))
			Expect(result.Condition).To(Equal(condition.Type))
			Expect(result.EligibleAt).To(Equal(now.Add(10 * time.Minute)))

			result = evaluate(matcher, now.Add(35*time.Minute), condition)
			Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
			Expect(result.Condition).To(Equal(condition.Type))
			Expect(result.EligibleAt).To(Equal(now.Add(10 * time.Minute)))
			Expect(result.Score).To(BeNumerically("~", 25.0/30.0))
		})

		It("retains the earliest eligibility across same-action policies", func() {
			sameActionPolicies := []cloudprovider.RepairPolicy{
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
			reversed := slices.Clone(sameActionPolicies)
			slices.Reverse(reversed)
			for _, orderedPolicies := range [][]cloudprovider.RepairPolicy{sameActionPolicies, reversed} {
				sameActionMatcher := newMatcher(orderedPolicies)
				result := evaluate(sameActionMatcher, now.Add(25*time.Minute), condition)
				Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
				Expect(result.EligibleAt).To(Equal(now.Add(10 * time.Minute)))
			}
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
				gracePeriodMatcher := newMatcher(orderedPolicies)

				result := evaluate(gracePeriodMatcher, now, condition)
				Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
				Expect(result.TerminationGracePeriod).NotTo(BeNil())
				Expect(*result.TerminationGracePeriod).To(Equal(shortGracePeriod))
			}
		})

		It("uses the default fallback across supported conditions", func() {
			crossConditionMatcher := newMatcher([]cloudprovider.RepairPolicy{
				defaultFallback,
				{
					ConditionType:   "StorageReady",
					ConditionStatus: corev1.ConditionFalse,
					ReasonRegex:     "^KnownStorageFailure$",
					Action:          cloudprovider.RebootNode,
				},
			})

			storageCondition := condition
			storageCondition.Type = "StorageReady"
			storageCondition.Reason = "NewStorageFailure"
			result := evaluate(crossConditionMatcher, now.Add(31*time.Minute), storageCondition)

			Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
			Expect(result.Condition).To(Equal(corev1.NodeConditionType("StorageReady")))
			Expect(result.EligibleAt).To(Equal(now.Add(30 * time.Minute)))
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
			matchAllMatcher := newMatcher(policies)

			Expect(evaluate(matchAllMatcher, now, condition).Action).To(Equal(cloudprovider.RebootNode))
		})

		It("does not match a different condition type or status", func() {
			differentType := condition
			differentType.Type = "DifferentCondition"
			Expect(evaluate(matcher, now, differentType).Action).To(BeEmpty())

			differentStatus := condition
			differentStatus.Status = corev1.ConditionTrue
			Expect(evaluate(matcher, now, differentStatus).Action).To(BeEmpty())
		})

		It("scores a node using its most urgent eligible policy", func() {
			nodeMatcher := newMatcher([]cloudprovider.RepairPolicy{
				{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
				{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
				{ConditionType: "Fallback", ConditionStatus: corev1.ConditionFalse, Action: cloudprovider.ReplaceNode},
			})
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
			Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
			Expect(result.Condition).To(Equal(corev1.NodeConditionType("HighPriority")))
		})

		It("merges score, action, condition, eligibility, and TGP across conditions", func() {
			rebootGracePeriod := 5 * time.Minute
			replaceGracePeriod := 15 * time.Minute
			nodeMatcher := newMatcher([]cloudprovider.RepairPolicy{
				{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, TerminationGracePeriod: &rebootGracePeriod, Action: cloudprovider.RebootNode},
				{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 10, TerminationGracePeriod: &replaceGracePeriod, Action: cloudprovider.ReplaceNode},
				{ConditionType: "Fallback", ConditionStatus: corev1.ConditionFalse, Action: cloudprovider.ReplaceNode},
			})
			highPriority := corev1.NodeCondition{
				Type:               "HighPriority",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(now.Add(-180 * time.Minute)),
			}
			lowPriority := corev1.NodeCondition{
				Type:               "LowPriority",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(now.Add(-45 * time.Minute)),
			}

			for _, conditions := range [][]corev1.NodeCondition{{highPriority, lowPriority}, {lowPriority, highPriority}} {
				result := evaluate(nodeMatcher, now, conditions...)
				Expect(result.Score).To(Equal(float64(7)))
				Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
				Expect(result.Condition).To(Equal(corev1.NodeConditionType("LowPriority")))
				Expect(result.EligibleAt).To(Equal(now.Add(-150 * time.Minute)))
				Expect(result.TerminationGracePeriod).NotTo(BeNil())
				Expect(*result.TerminationGracePeriod).To(Equal(rebootGracePeriod))
			}
		})

		It("uses earliest eligibility to break equal-priority condition ties", func() {
			nodeMatcher := newMatcher([]cloudprovider.RepairPolicy{
				{ConditionType: "Earlier", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 20 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
				{ConditionType: "Later", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 10 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
				{ConditionType: "Fallback", ConditionStatus: corev1.ConditionFalse, Action: cloudprovider.ReplaceNode},
			})
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
				result := evaluate(nodeMatcher, now, conditions...)
				Expect(result.Condition).To(Equal(corev1.NodeConditionType("Earlier")))
			}
		})

		It("uses condition type to break complete policy ties", func() {
			nodeMatcher := newMatcher([]cloudprovider.RepairPolicy{
				{ConditionType: "ConditionA", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", Priority: 50, Action: cloudprovider.ReplaceNode},
				{ConditionType: "ConditionB", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", Priority: 50, Action: cloudprovider.ReplaceNode},
				{ConditionType: "Fallback", ConditionStatus: corev1.ConditionFalse, Action: cloudprovider.ReplaceNode},
			})
			conditionA := corev1.NodeCondition{Type: "ConditionA", Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now)}
			conditionB := corev1.NodeCondition{Type: "ConditionB", Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now)}

			for _, conditions := range [][]corev1.NodeCondition{{conditionA, conditionB}, {conditionB, conditionA}} {
				Expect(evaluate(nodeMatcher, now, conditions...).Condition).To(Equal(corev1.NodeConditionType("ConditionA")))
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
			initialMatcher := newMatcher(reasonPolicies)
			Expect(evaluate(initialMatcher, now, condition).Action).To(BeEmpty())

			condition.Reason = "ImmediateReason"
			restartedMatcher := newMatcher(reasonPolicies)
			result := evaluate(restartedMatcher, now, condition)
			Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
			Expect(result.EligibleAt).To(Equal(now.Add(-30 * time.Minute)))
		})
	})

})
