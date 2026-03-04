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
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// PolicyEvaluator determines if a consolidation command should be executed based on the ConsolidateWhen policy
type PolicyEvaluator struct{}

// ShouldConsolidate returns true if the command should be executed based on the policy
// The threshold parameter is used when policy is WhenCostJustifiesDisruption
func (e *PolicyEvaluator) ShouldConsolidate(
	policy v1.ConsolidateWhenPolicy,
	candidate *Candidate,
	decisionRatio float64,
	threshold float64,
) bool {
	switch policy {
	case v1.ConsolidateWhenEmpty:
		return len(candidate.reschedulablePods) == 0
	case v1.ConsolidateWhenEmptyOrUnderutilized:
		return true // No filtering
	case v1.ConsolidateWhenCostJustifiesDisruption:
		return decisionRatio >= threshold
	default:
		return true // Default to no filtering for backward compatibility
	}
}
