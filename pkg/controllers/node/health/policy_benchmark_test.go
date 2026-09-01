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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var benchmarkRepairPolicyResult *RepairPolicyResult

func BenchmarkRepairPolicyMatcherEvaluate(b *testing.B) {
	for _, policyCount := range []int{1, 10, 100} {
		for _, matching := range []bool{false, true} {
			b.Run(fmt.Sprintf("SpecificPolicies=%d/Matching=%t", policyCount, matching), func(b *testing.B) {
				policies := make([]cloudprovider.RepairPolicy, 0, policyCount+1)
				for range policyCount {
					reasonRegex := "^other$"
					if matching {
						reasonRegex = "^failure$"
					}
					policies = append(policies, cloudprovider.RepairPolicy{
						ConditionType:      "AcceleratorReady",
						ConditionStatus:    corev1.ConditionFalse,
						ReasonRegex:        reasonRegex,
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
					b.Fatal(err)
				}
				now := time.Now()
				condition := corev1.NodeCondition{
					Type:               "AcceleratorReady",
					Status:             corev1.ConditionFalse,
					Reason:             "failure",
					LastTransitionTime: metav1.NewTime(now.Add(-time.Hour)),
				}

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					benchmarkRepairPolicyResult = matcher.Evaluate(condition, now)
				}
			})
		}
	}
}
