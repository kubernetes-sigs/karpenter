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

// Helpers shared by the three Balanced consolidation contexts. Each context
// lives in its own spec file, matching the rest of this directory, so the
// shared pieces sit here alongside report.go and thresholds.go.

// writeLatencySidecar emits result to OUTPUT_DIR/<filePrefix>_latency.json via
// the shared common.WriteLatencySidecar helper. Logs summary counts to
// GinkgoWriter regardless of OUTPUT_DIR so CI logs surface the harness result.
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
	// The sidecar is the artifact this harness exists to produce and e2e.yaml
	// uploads it, so a write failure is a spec failure rather than a log line.
	// WriteLatencySidecar is a no-op when OUTPUT_DIR is unset, which is how the
	// suite runs locally.
	Expect(common.WriteLatencySidecar(os.Getenv("OUTPUT_DIR"), filePrefix, sc)).To(Succeed())
}

// emitPolicyRun writes both the PerformanceReport JSON and the latency
// sidecar JSON under a shared file prefix. The two artifacts always stay
// paired on disk for offline diff analysis.
func emitPolicyRun(report *PerformanceReport, filePrefix string, policy v1.ConsolidationPolicy, result *common.LatencyResult) {
	OutputPerformanceReport(report, filePrefix)
	writeLatencySidecar(report.TestName, filePrefix, policy, result)
}

// scoreBucketBelowThreshold is the karpenter_consolidation_score bucket bound
// just below the 1/k=0.5 Balanced threshold.
const scoreBucketBelowThreshold = 0.33

// expectBalancedDecisionsMatchThreshold fails if Balanced scored no moves, or
// if any recorded decision disagrees with the 1/k threshold: approved scores
// (>= 0.5) must land above the 0.33 bucket and rejected scores (< 0.5) at or
// below the 0.5 bucket. It returns the set of NodePools that scored a move.
//
// Read the limits before trusting this.
//
// 1. It is partly self-referential. The decision label is written from the
// same predicate it checks: ApproveCommand labels a move "approved" exactly
// when result.Score() >= result.Threshold(), for whatever K is in effect. So
// consistency between the label and the score holds by construction, and what
// the assertion really pins is the bucket the score landed in.
//
// 2. Resolution limit on the approved arm. Min and Max come from bucket
// bounds, not raw observations, and the score buckets are
// {0.1, 0.25, 0.33, 0.5, 1.0, 2.0, 5.0, 10.0}. A correctly approved score
// (>= 0.5) lands in the le=0.5 bucket whose lower bound is 0.33, so Min is
// 0.33. A wrongly approved score anywhere in (0.33, 0.5) lands in the same
// bucket and reports the same Min. Combined with point 1, a BalancedK of 3
// (threshold 0.3334) passes both arms: approved scores still sit above 0.33
// and rejected scores still sit at or below 0.33. The blind interval on the
// threshold is (0.33, 0.5], so k in [2, 3.03).
//
// 3. Neither arm requires a rejection to occur, so a regression that approves
// every move passes as long as the approved scores are genuinely >= 0.33.
// Requiring a non-zero rejection count would close that, but these fixtures
// do not force scores near the boundary, so it would trade a blind spot for a
// flake.
//
// What it does catch, unambiguously: Balanced not taking effect at all. The
// score histogram is observed only inside balancedEvaluator, gated on the
// NodePool's policy being Balanced, and the policy label is read back from the
// NodePool the controller reconciled. If Balanced silently fell back, no
// series carries policy="Balanced", scored stays 0 and this fails. That
// read-back is the only thing in the suite proving the Balanced code path ran.
// It also catches a K of 1, 4 or 10, and a metric or label rename.
//
// Closing point 2 needs the exact score rather than a bucket. Only the
// ConsolidationApproved event carries it.
func expectBalancedDecisionsMatchThreshold(result *common.LatencyResult) sets.Set[string] {
	threshold := 1.0 / float64(v1.BalancedK)
	scored := uint64(0)
	pools := sets.New[string]()
	for key, s := range result.LatencyStats {
		if s.MetricName != "karpenter_consolidation_score" || s.Count == 0 || s.Labels["policy"] != string(v1.ConsolidationPolicyBalanced) {
			continue
		}
		// DecisionDim also declares no-op, replace and delete. Balanced only
		// ever emits approved or rejected here, so anything else means the
		// emission side changed and this assertion stopped covering it.
		switch decision := s.Labels["decision"]; decision {
		case "approved":
			Expect(s.Min).To(BeNumerically(">=", scoreBucketBelowThreshold), "%s: approved a move scoring below the %.2f threshold", key, threshold)
		case "rejected":
			Expect(s.Max).To(BeNumerically("<=", threshold), "%s: rejected a move scoring above the %.2f threshold", key, threshold)
		default:
			Fail(fmt.Sprintf("%s: unexpected decision label %q on a Balanced consolidation score; this assertion no longer covers it", key, decision))
		}
		// Counted after the switch so an unrecognized decision cannot satisfy
		// the scored > 0 check below.
		scored += s.Count
		pools.Insert(s.Labels["nodepool"])
	}
	Expect(scored).To(BeNumerically(">", 0), "Balanced recorded no scored consolidation moves")
	GinkgoWriter.Printf("Balanced scored %d consolidation moves across nodepools %v\n", scored, sets.List(pools))
	return pools
}
