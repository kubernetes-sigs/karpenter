//go:build !race

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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func TestRepairPolicyMatcherHotPathsDoNotAllocatePerPolicy(t *testing.T) {
	policies := make([]cloudprovider.RepairPolicy, 0, 101)
	for range 100 {
		policies = append(policies, cloudprovider.RepairPolicy{
			ConditionType:      "AcceleratorReady",
			ConditionStatus:    corev1.ConditionFalse,
			ReasonRegex:        "^failure$",
			TolerationDuration: time.Minute,
			Action:             cloudprovider.ReplaceNode,
		})
	}
	policies = append(policies, cloudprovider.RepairPolicy{
		ConditionType:      "AcceleratorReady",
		ConditionStatus:    corev1.ConditionFalse,
		TolerationDuration: time.Minute,
		Action:             cloudprovider.ReplaceNode,
	})
	matcher, err := NewRepairPolicyMatcher(policies, sets.New(cloudprovider.ReplaceNode))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	condition := corev1.NodeCondition{
		Type:               "AcceleratorReady",
		Status:             corev1.ConditionFalse,
		Reason:             "failure",
		LastTransitionTime: metav1.NewTime(now.Add(-time.Hour)),
	}
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{condition}}}

	allocations := testing.AllocsPerRun(1000, func() {
		benchmarkRepairPolicyResult = matcher.Evaluate(node, now)
	})
	if benchmarkRepairPolicyResult.Decision == nil {
		t.Fatal("expected an eligible repair policy result")
	}
	if allocations > 3 {
		t.Fatalf("expected fixed result, evaluation, and scoring-slice allocations, got %.2f allocations", allocations)
	}

	allocations = testing.AllocsPerRun(1000, func() {
		benchmarkRepairPolicyMatches = matcher.Matches(condition)
	})
	if !benchmarkRepairPolicyMatches {
		t.Fatal("expected the repair policy to cover the condition")
	}
	if allocations != 0 {
		t.Fatalf("expected checking condition coverage not to allocate, got %.2f allocations", allocations)
	}
}

var benchmarkRepairPolicyMatches bool
