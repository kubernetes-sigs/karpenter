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

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
)

func TestRepairPolicyLogValues(t *testing.T) {
	eligibleAt := time.Date(2026, time.September, 22, 12, 30, 0, 0, time.UTC)
	terminationGracePeriod := 5 * time.Minute
	result := &health.RepairPolicyEvaluation{
		ConditionType:          "AcceleratorReady",
		ConditionStatus:        corev1.ConditionFalse,
		Reason:                 "NvidiaXID48Error",
		Action:                 cloudprovider.ReplaceNode,
		EligibleAt:             eligibleAt,
		TerminationGracePeriod: &terminationGracePeriod,
		Fallback:               false,
		MatchingPolicies:       2,
		EligiblePolicies:       1,
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
