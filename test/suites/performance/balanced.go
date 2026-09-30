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

package performance

import (
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

func writeLatencySidecar(testName, filePrefix string, policy v1.ConsolidationPolicy, result *common.LatencyResult) {
	if result == nil {
		GinkgoWriter.Printf("LatencyHarness: nil result for %s (%s); skipping sidecar\n", testName, policy)
		return
	}
	GinkgoWriter.Printf("LatencyHarness [%s, %s]: %d histogram series, %d counter series\n",
		testName, policy, len(result.LatencyStats), len(result.Counters))
	sc := common.LatencySidecar{
		TestName:            testName,
		ConsolidationPolicy: string(policy),
		Timestamp:           time.Now(),
		LatencyStats:        result.LatencyStats,
		Counters:            result.Counters,
	}
	Expect(common.WriteLatencySidecar(os.Getenv("OUTPUT_DIR"), filePrefix, sc)).To(Succeed())
}

func emitPolicyRun(report *PerformanceReport, filePrefix string, policy v1.ConsolidationPolicy, result *common.LatencyResult) {
	OutputPerformanceReport(report, filePrefix)
	writeLatencySidecar(report.TestName, filePrefix, policy, result)
}

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
