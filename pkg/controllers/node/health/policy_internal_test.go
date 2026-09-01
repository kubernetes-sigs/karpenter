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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
)

var _ = Describe("Repair Policies", func() {
	supportedActions := sets.New(cloudprovider.RebootNode, cloudprovider.ReplaceNode)
	validFallback := cloudprovider.RepairPolicy{
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
			[]cloudprovider.RepairPolicy{validSpecific, validFallback},
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
					Action:          cloudprovider.ReplaceNode,
				},
				{
					ConditionType:   "ConditionUnknown",
					ConditionStatus: corev1.ConditionUnknown,
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
				validFallback,
			},
			supportedActions,
			"invalid reason regex",
		),
		Entry("rejects an unsupported action",
			[]cloudprovider.RepairPolicy{validSpecific, validFallback},
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
		Entry("rejects a missing fallback",
			[]cloudprovider.RepairPolicy{validSpecific},
			supportedActions,
			"exactly one condition-level fallback, found 0",
		),
		Entry("rejects multiple fallbacks",
			[]cloudprovider.RepairPolicy{validFallback, validFallback},
			supportedActions,
			"exactly one condition-level fallback, found 2",
		),
		Entry("requires a replacement fallback",
			[]cloudprovider.RepairPolicy{{
				ConditionType:   "AcceleratorReady",
				ConditionStatus: corev1.ConditionFalse,
				Action:          cloudprovider.RebootNode,
			}},
			supportedActions,
			`condition-level fallback must use action "ReplaceNode"`,
		),
	)

	It("reports fallback validation errors in first-seen condition order", func() {
		policies := []cloudprovider.RepairPolicy{
			{
				ConditionType:   "ZCondition",
				ConditionStatus: corev1.ConditionFalse,
				ReasonRegex:     "reason",
				Action:          cloudprovider.ReplaceNode,
			},
			{
				ConditionType:   "ACondition",
				ConditionStatus: corev1.ConditionFalse,
				ReasonRegex:     "reason",
				Action:          cloudprovider.ReplaceNode,
			},
		}

		_, err := NewRepairPolicyMatcher(policies, supportedActions)
		Expect(err).To(HaveOccurred())
		zIndex := strings.Index(err.Error(), "condition=ZCondition")
		aIndex := strings.Index(err.Error(), "condition=ACondition")
		Expect(zIndex).To(BeNumerically(">=", 0))
		Expect(aIndex).To(BeNumerically(">", zIndex))
	})

	Context("Matching", func() {
		var now time.Time
		var condition corev1.NodeCondition
		var policies []cloudprovider.RepairPolicy
		var matcher *RepairPolicyMatcher
		evaluate := func(matcher *RepairPolicyMatcher, condition corev1.NodeCondition, now time.Time) *repairDecision {
			decision, ok := matcher.evaluateDecision(condition, now)
			if !ok {
				return nil
			}
			return &decision
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
			Expect(decision.eligible).To(BeFalse())
			Expect(decision.fallback).To(BeFalse())
			Expect(decision.matchingPolicies).To(Equal(2))
			Expect(decision.eligibleAt).To(Equal(now.Add(10 * time.Minute)))
			Expect(decision.logValues()).To(Equal([]any{
				"condition", corev1.NodeConditionType("AcceleratorReady"),
				"status", corev1.ConditionFalse,
				"reason", "NvidiaXID48Error",
				"fallback", false,
				"matching-policies", 2,
				"eligible-policies", 0,
				"action", cloudprovider.RebootNode,
				"eligible", false,
				"eligible-at", now.Add(10 * time.Minute),
			}))
		})

		It("merges independently eligible specific policies by action", func() {
			decision := evaluate(matcher, condition, now.Add(15*time.Minute))
			Expect(decision).NotTo(BeNil())
			Expect(decision.action).To(Equal(cloudprovider.RebootNode))
			Expect(decision.eligiblePolicies).To(Equal(1))
			Expect(decision.eligibleAt).To(Equal(now.Add(10 * time.Minute)))

			decision = evaluate(matcher, condition, now.Add(35*time.Minute))
			Expect(decision.action).To(Equal(cloudprovider.ReplaceNode))
			Expect(decision.eligiblePolicies).To(Equal(2))
			Expect(decision.eligibleAt).To(Equal(now.Add(30 * time.Minute)))
		})

		It("exposes only eligible per-condition results", func() {
			Expect(matcher.Evaluate(condition, now.Add(5*time.Minute))).To(BeNil())
			Expect(matcher.Evaluate(condition, now.Add(15*time.Minute))).To(Equal(&RepairPolicyResult{
				ConditionType:   condition.Type,
				ConditionStatus: condition.Status,
				Reason:          condition.Reason,
				Action:          cloudprovider.RebootNode,
				EligibleAt:      now.Add(10 * time.Minute),
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
			sameActionMatcher, err := NewRepairPolicyMatcher(policies, supportedActions)
			Expect(err).NotTo(HaveOccurred())

			decision := evaluate(sameActionMatcher, condition, now.Add(25*time.Minute))
			Expect(decision).NotTo(BeNil())
			Expect(decision.eligiblePolicies).To(Equal(2))
			Expect(decision.eligibleAt).To(Equal(now.Add(10 * time.Minute)))
		})

		It("uses the fallback for an unknown reason", func() {
			condition.Reason = "NewFailureCode"
			decision := evaluate(matcher, condition, now)
			Expect(decision).NotTo(BeNil())
			Expect(decision.eligible).To(BeTrue())
			Expect(decision.fallback).To(BeTrue())
			Expect(decision.action).To(Equal(cloudprovider.ReplaceNode))
			Expect(decision.matchingPolicies).To(Equal(1))
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
			Expect(decision.fallback).To(BeFalse())
			Expect(decision.action).To(Equal(cloudprovider.RebootNode))
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
			Expect(forwardDecision.action).To(Equal(cloudprovider.ReplaceNode))
			Expect(evaluate(reversedMatcher, condition, now.Add(5*time.Minute))).To(Equal(forwardDecision))
		})

		It("reconstructs eligibility from the current condition after restart", func() {
			Expect(evaluate(matcher, condition, now.Add(15*time.Minute)).action).To(Equal(cloudprovider.RebootNode))
			condition.Reason = "NewFailureCode"

			restartedMatcher, err := NewRepairPolicyMatcher(policies, supportedActions)
			Expect(err).NotTo(HaveOccurred())
			Expect(evaluate(restartedMatcher, condition, now.Add(35*time.Minute))).To(
				Equal(evaluate(matcher, condition, now.Add(35*time.Minute))),
			)
		})
	})

	It("rejects reboot policies until the reboot lifecycle is available", func() {
		now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
		cloudProvider := fake.NewCloudProvider()
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			validSpecific,
			validFallback,
		}

		_, err := NewController(nil, cloudProvider, clocktesting.NewFakeClock(now), nil)
		Expect(err).To(MatchError(ContainSubstring(`unsupported action "RebootNode"`)))
	})
})

var _ = Describe("Node Health Predicate", func() {
	var controller *Controller
	var oldNode *corev1.Node

	BeforeEach(func() {
		matcher, err := NewRepairPolicyMatcher([]cloudprovider.RepairPolicy{
			{
				ConditionType:   "AcceleratorReady",
				ConditionStatus: corev1.ConditionFalse,
				ReasonRegex:     "^NvidiaXID",
				Action:          cloudprovider.ReplaceNode,
			},
			{
				ConditionType:   "AcceleratorReady",
				ConditionStatus: corev1.ConditionFalse,
				Action:          cloudprovider.ReplaceNode,
			},
			{
				ConditionType:   corev1.NodeReady,
				ConditionStatus: corev1.ConditionFalse,
				Action:          cloudprovider.ReplaceNode,
			},
		}, sets.New(cloudprovider.ReplaceNode))
		Expect(err).NotTo(HaveOccurred())
		controller = &Controller{repairPolicyMatcher: matcher}
		oldNode = &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:               "AcceleratorReady",
			Status:             corev1.ConditionFalse,
			Reason:             "NvidiaXID48Error",
			LastTransitionTime: metav1.NewTime(time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)),
		}}}}
	})

	It("reconciles reason-only changes for conditions with reason policies", func() {
		newNode := oldNode.DeepCopy()
		newNode.Status.Conditions[0].Reason = "NvidiaXID63Error"

		Expect(controller.nodeHealthChanged(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode})).To(BeTrue())
	})

	It("reconciles status and transition-time changes", func() {
		newNode := oldNode.DeepCopy()
		newNode.Status.Conditions[0].Status = corev1.ConditionTrue
		Expect(controller.nodeHealthChanged(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode})).To(BeTrue())

		newNode = oldNode.DeepCopy()
		newNode.Status.Conditions[0].LastTransitionTime = metav1.NewTime(oldNode.Status.Conditions[0].LastTransitionTime.Add(time.Minute))
		Expect(controller.nodeHealthChanged(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode})).To(BeTrue())
	})

	It("reconciles added and replaced conditions", func() {
		newNode := oldNode.DeepCopy()
		newNode.Status.Conditions = append(newNode.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeReady})
		Expect(controller.nodeHealthChanged(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode})).To(BeTrue())

		newNode = oldNode.DeepCopy()
		newNode.Status.Conditions[0].Type = corev1.NodeReady
		Expect(controller.nodeHealthChanged(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode})).To(BeTrue())
	})

	It("ignores reason-only changes for unrelated and fallback-only conditions", func() {
		newNode := oldNode.DeepCopy()
		newNode.Status.Conditions[0].Type = "UnrelatedCondition"
		newNode.Status.Conditions[0].Reason = "NewReason"
		oldNode.Status.Conditions[0].Type = "UnrelatedCondition"
		Expect(controller.nodeHealthChanged(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode})).To(BeFalse())

		oldNode.Status.Conditions[0].Type = corev1.NodeReady
		newNode.Status.Conditions[0].Type = corev1.NodeReady
		Expect(controller.nodeHealthChanged(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode})).To(BeFalse())
	})

	It("ignores unchanged conditions", func() {
		Expect(controller.nodeHealthChanged(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: oldNode.DeepCopy()})).To(BeFalse())
	})
})
