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

// Helpers shared by the three Balanced consolidation specs.
// expectBalancedDecisionsMatchThreshold checks that every scored Balanced move
// the run recorded sits on the right side of the 1/BalancedK threshold, to the
// resolution the consolidation_score bucket layout allows: the approved arm
// compares Min against the bucket bound below the threshold, so it cannot
// distinguish an approval at 0.34 from one at 0.5.

package performance

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

const scoreBucketBelowThreshold = 0.33

func expectBalancedDecisionsMatchThreshold(result *common.LatencyResult) sets.Set[string] {
	threshold := 1.0 / float64(v1.BalancedK)
	scored := uint64(0)
	pools := sets.New[string]()
	for key, s := range result.LatencyStats {
		if s.MetricName != "karpenter_consolidation_score" || s.Count == 0 || s.Labels["policy"] != string(v1.ConsolidationPolicyBalanced) {
			continue
		}
		switch decision := s.Labels["decision"]; decision {
		case "approved":
			Expect(s.Min).To(BeNumerically(">=", scoreBucketBelowThreshold), "%s: approved a move scoring below the %.2f threshold", key, threshold)
		case "rejected":
			Expect(s.Max).To(BeNumerically("<=", threshold), "%s: rejected a move scoring above the %.2f threshold", key, threshold)
		default:
			Fail(fmt.Sprintf("%s: unexpected decision label %q on a Balanced consolidation score; this assertion no longer covers it", key, decision))
		}
		scored += s.Count
		pools.Insert(s.Labels["nodepool"])
	}
	Expect(scored).To(BeNumerically(">", 0), "Balanced recorded no scored consolidation moves")
	GinkgoWriter.Printf("Balanced scored %d consolidation moves across nodepools %v\n", scored, sets.List(pools))
	return pools
}
