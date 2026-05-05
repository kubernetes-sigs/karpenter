//go:build test_aspirational

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

package aspirational

import (
	"testing"
)

// TestDriftDoesNotStarveConsolidationBudget documents the scenario from
// https://github.com/kubernetes-sigs/karpenter/pull/2930
//
// When nodes are drifted, drift disruption can consume the entire disruption
// budget, leaving consolidation unable to make progress indefinitely. The
// desired behavior is fair sharing of the disruption budget between drift and
// consolidation so that cost optimization can still proceed during rolling
// drift events.
//
// This test will pass once disruption budget fairness is implemented.
func TestDriftDoesNotStarveConsolidationBudget(t *testing.T) {
	t.Skip("aspirational: blocked on disruption budget fairness RFC (#2930)")
}
