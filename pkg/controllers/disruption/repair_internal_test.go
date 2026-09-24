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
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
)

func TestRepairPolicyLogValues(t *testing.T) {
	eligibleAt := time.Date(2026, time.September, 22, 12, 30, 0, 0, time.UTC)
	terminationGracePeriod := 5 * time.Minute
	result := &health.RepairPolicyResult{
		ConditionType:          "AcceleratorReady",
		ConditionStatus:        corev1.ConditionFalse,
		Reason:                 "NvidiaXID48Error",
		Action:                 cloudprovider.ReplaceNode,
		EligibleAt:             eligibleAt,
		TerminationGracePeriod: &terminationGracePeriod,
		Fallback:               false,
		MatchingPolicies:       2,
		EligiblePolicies:       make([]health.EligibleRepairPolicy, 1),
	}
	expected := []any{
		"condition", corev1.NodeConditionType("AcceleratorReady"),
		"status", corev1.ConditionFalse,
		"reason", "NvidiaXID48Error",
		"fallback", false,
		"matching-policies", 2,
		"eligible-policies", 1,
		"action", cloudprovider.ReplaceNode,
		"eligible", true,
		"eligible-at", eligibleAt,
		"termination-grace-period", terminationGracePeriod,
	}
	if actual := repairPolicyLogValues(result); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected log values %#v, got %#v", expected, actual)
	}
}

func TestEvaluateNodeUsesMaximumEligiblePolicyScore(t *testing.T) {
	now := time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)
	policies := []cloudprovider.RepairPolicy{
		{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
		{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
		{ConditionType: "Fallback", ConditionStatus: corev1.ConditionFalse, Action: cloudprovider.ReplaceNode},
	}
	matcher, err := health.NewRepairPolicyMatcher(policies, sets.New(cloudprovider.ReplaceNode))
	if err != nil {
		t.Fatalf("creating repair policy matcher, %v", err)
	}
	repair := &Repair{
		policyMatcher: matcher,
		ranks:         denseRanks(policies),
	}
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

	evaluation := repair.evaluateNode(node, now)
	if evaluation.score != 6 {
		t.Fatalf("expected maximum eligible score 6, got %v", evaluation.score)
	}
	if evaluation.result == nil || evaluation.result.ConditionType != "HighPriority" {
		t.Fatalf("expected the high-priority condition to govern the action, got %#v", evaluation.result)
	}
}

func TestEvaluateNodeUsesEarliestEligibleAtForEqualPriorityConditions(t *testing.T) {
	now := time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)
	policies := []cloudprovider.RepairPolicy{
		{ConditionType: "Earlier", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 20 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
		{ConditionType: "Later", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 10 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
		{ConditionType: "Fallback", ConditionStatus: corev1.ConditionFalse, Action: cloudprovider.ReplaceNode},
	}
	matcher, err := health.NewRepairPolicyMatcher(policies, sets.New(cloudprovider.ReplaceNode))
	if err != nil {
		t.Fatalf("creating repair policy matcher, %v", err)
	}
	repair := &Repair{
		policyMatcher: matcher,
		ranks:         denseRanks(policies),
	}
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
	for _, conditions := range [][]corev1.NodeCondition{
		{earlier, later},
		{later, earlier},
	} {
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: conditions}}
		evaluation := repair.evaluateNode(node, now)
		if evaluation.result == nil || evaluation.result.ConditionType != "Earlier" {
			t.Fatalf("expected the earliest eligible result to govern for conditions %#v, got %#v", conditions, evaluation.result)
		}
	}
}
