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

package scenarios

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// The report keeps a histogram's bucket edges, not its samples, so the check works in buckets of
// karpenter_consolidation_score (pkg/controllers/disruption/metrics.go): an approved move's lowest bucket must start
// at or above scoreBucketBelowThreshold, the edge just below 1/BalancedK, and a rejected move's highest bucket must
// end at or below 1/BalancedK, which must itself be an edge.
const (
	scoreBucketBelowThreshold = 0.33
	scoreThresholdBucket      = 0.5
)

// expectBalancedDecisions checks every Balanced consolidation score the phase recorded against the policy's
// threshold, and returns the NodePools that scored moves. A skipped phase has no Result and isn't checked.
func expectBalancedDecisions(rep *perf.Result) sets.Set[string] {
	GinkgoHelper()
	pools := sets.New[string]()
	if rep == nil {
		return pools
	}
	threshold := 1.0 / float64(v1.BalancedK)
	Expect(threshold).To(Equal(scoreThresholdBucket), "1/BalancedK moved off the consolidation_score bucket edges this check assumes; update them")
	scored := uint64(0)
	for _, s := range rep.Histograms("karpenter_consolidation_score", map[string]string{"policy": string(v1.ConsolidationPolicyBalanced)}) {
		switch decision := s.Labels["decision"]; decision {
		case "approved":
			Expect(s.Min).To(BeNumerically(">=", scoreBucketBelowThreshold), "%v: approved a move scoring below the %.2f threshold", s.Labels, threshold)
		case "rejected":
			Expect(s.Max).To(BeNumerically("<=", threshold), "%v: rejected a move scoring above the %.2f threshold", s.Labels, threshold)
		default:
			Fail(fmt.Sprintf("%v: unexpected decision %q on a Balanced consolidation score; this check no longer covers it", s.Labels, decision))
		}
		scored += s.Count
		pools.Insert(s.Labels["nodepool"])
	}
	Expect(scored).To(BeNumerically(">", 0), "Balanced recorded no scored consolidation moves")
	return pools
}

func balancedPool(r *perf.Run) {
	r.NodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
}
