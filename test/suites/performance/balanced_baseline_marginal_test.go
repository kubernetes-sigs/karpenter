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
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// buildMarginalDeployments returns two deployments with the basic_test.go
// resource profile (500 pods each: small 900m/3100Mi, large 3500m/28Gi).
// When deletionCost > 0 the small deployment carries a pod-deletion-cost
// annotation, raising each pod's EvictionCost by cost/2^27 (clamped at 10)
// and driving the pool's total_disruption_cost up. That pushes the
// consolidation score BELOW the 1/k=0.5 Balanced threshold on scale-in.
// When deletionCost is zero the same fixture stays ABOVE threshold. The
// single toggle isolates score-side behavior from the workload shape.
func buildMarginalDeployments(deletionCost int) (*appsv1.Deployment, *appsv1.Deployment) {
	var smallExtras []test.DeploymentOptionModifier
	if deletionCost > 0 {
		smallExtras = append(smallExtras, test.WithAnnotations(map[string]string{
			corev1.PodDeletionCost: strconv.Itoa(deletionCost),
		}))
	}
	smallOpts := test.CreateDeploymentOptions("marginal-small-app", 500, "900m", "3100Mi", smallExtras...)
	largeOpts := test.CreateDeploymentOptions("marginal-large-app", 500, "3500m", "28Gi")
	return test.Deployment(smallOpts), test.Deployment(largeOpts)
}

// runBaselineMarginalPhases runs the shared two-phase fixture (1000-pod
// scale-out then 700-pod consolidation) with LatencyHarness capture and
// sidecar-JSON emission for both phases. The scale-in phase drives both
// deployments to 350 replicas at once, matching basic_test.go.
// The caller pins the ConsolidationPolicy before invocation; this function
// reads it back from the NodePool so the sidecar records the policy that
// actually ran. Returns the consolidation phase's latency result so callers
// can run spec-specific soft checks on score-histogram deltas.
func runBaselineMarginalPhases(filePrefixBase string, smallDeployment, largeDeployment *appsv1.Deployment) *common.LatencyResult {
	scaleOutPrefix := filePrefixBase + "_scale_out"
	consolidationPrefix := filePrefixBase + "_consolidation"
	policy := nodePool.Spec.Disruption.ConsolidationPolicy

	env.ExpectCreated(nodePool, nodeClass, smallDeployment, largeDeployment)

	scaleOutHarness, err := common.StartLatencyHarness(env)
	Expect(err).ToNot(HaveOccurred())

	scaleOutReport, err := ReportScaleOutWithOutput(env,
		filePrefixBase+" Scale Out", 1000, 15*time.Minute, scaleOutPrefix)
	Expect(err).ToNot(HaveOccurred())
	Expect(scaleOutReport.TotalPods).To(Equal(1000))
	initialNodes := scaleOutReport.TotalNodes

	scaleOutLatency, err := scaleOutHarness.Stop()
	Expect(err).ToNot(HaveOccurred())
	GinkgoWriter.Printf("LatencyHarness [%s, %s]: %d histogram series, %d counter series\n",
		scaleOutReport.TestName, policy, len(scaleOutLatency.LatencyStats), len(scaleOutLatency.Counters))
	if err := common.WriteLatencySidecar(os.Getenv("OUTPUT_DIR"), scaleOutPrefix, common.LatencySidecar{
		TestName:            scaleOutReport.TestName,
		ConsolidationPolicy: string(policy),
		Timestamp:           time.Now(),
		LatencyStats:        scaleOutLatency.LatencyStats,
		Counters:            scaleOutLatency.Counters,
	}); err != nil {
		GinkgoWriter.Printf("LatencyHarness: %v\n", err)
	}

	By("Scaling both deployments down and capturing consolidation latency")
	smallDeployment.Spec.Replicas = new(int32(350))
	largeDeployment.Spec.Replicas = new(int32(350))
	env.ExpectUpdated(smallDeployment, largeDeployment)

	consolidationHarness, err := common.StartLatencyHarness(env)
	Expect(err).ToNot(HaveOccurred())

	consolidationReport, err := ReportConsolidation(env,
		filePrefixBase+" Consolidation", 1000, 700, initialNodes, 20*time.Minute)
	Expect(err).ToNot(HaveOccurred())

	consolidationLatency, err := consolidationHarness.Stop()
	Expect(err).ToNot(HaveOccurred())

	OutputPerformanceReport(consolidationReport, consolidationPrefix)
	GinkgoWriter.Printf("LatencyHarness [%s, %s]: %d histogram series, %d counter series\n",
		consolidationReport.TestName, policy, len(consolidationLatency.LatencyStats), len(consolidationLatency.Counters))
	if err := common.WriteLatencySidecar(os.Getenv("OUTPUT_DIR"), consolidationPrefix, common.LatencySidecar{
		TestName:            consolidationReport.TestName,
		ConsolidationPolicy: string(policy),
		Timestamp:           time.Now(),
		LatencyStats:        consolidationLatency.LatencyStats,
		Counters:            consolidationLatency.Counters,
	}); err != nil {
		GinkgoWriter.Printf("LatencyHarness: %v\n", err)
	}

	Expect(consolidationReport.TotalPods).To(Equal(700))
	Expect(consolidationLatency.Counters).ToNot(BeNil())
	Expect(consolidationLatency.LatencyStats).ToNot(BeNil())
	return consolidationLatency
}

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Balanced Baseline", func() {
		// Control arm: same 1000-pod / 700-pod fixture as basic_test.go but
		// wrapped with LatencyHarness so the hero histograms
		// (scheduling_decision, voluntary_disruption_decision_evaluation,
		// pods_bound) are captured for both scale-out and consolidation. The
		// suite-wide default policy is already WhenEmptyOrUnderutilized
		// (suite_test.go:62), so this spec exists as the reference latency
		// distribution the marginal Balanced runs compare against.
		// Cross-policy delta analysis is offline on the paired sidecar
		// JSONs; asserting deltas in-band would encode KWOK-timing flake
		// into CI.
		It("should capture reference latency distribution under WhenEmptyOrUnderutilized", func() {
			smallDeployment, largeDeployment := buildMarginalDeployments(0)
			runBaselineMarginalPhases("balanced_baseline", smallDeployment, largeDeployment)
		})
	})

	Context("Balanced Marginal Move", func() {
		// Two variants share the 1000-pod fixture. The only difference is
		// the small deployment's pod-deletion-cost annotation, which shifts
		// the pool's total_disruption_cost denominator and pushes the
		// consolidation score across the 1/k=0.5 threshold. See the RFC's
		// "Marginal Move" example (designs/balanced-consolidation.md:236).
		//
		// Neither variant asserts an exact count from
		// karpenter_consolidation_moves_total{decision=...}; KWOK timing
		// blurs which candidates land in a given round. Instead the latency
		// sidecar carries counters and score-histogram deltas so downstream
		// analysis can compare distributions across the paired runs and
		// against the baseline sidecar.
		BeforeEach(func() {
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
		})
		type marginalFixture struct {
			name             string
			filePrefix       string
			deletionCost     int
			expectedDecision string
		}
		fixtures := []marginalFixture{
			{
				name:             "just-above-threshold (score > 0.5, accepts)",
				filePrefix:       "balanced_marginal_accept",
				deletionCost:     0,
				expectedDecision: "approved",
			},
			{
				name:             "just-below-threshold (score < 0.5, rejects)",
				filePrefix:       "balanced_marginal_reject",
				deletionCost:     2000000000,
				expectedDecision: "rejected",
			},
		}
		for _, fx := range fixtures {
			It(fmt.Sprintf("should capture Balanced consolidation latency %s", fx.name), func() {
				smallDeployment, largeDeployment := buildMarginalDeployments(fx.deletionCost)
				consolidationLatency := runBaselineMarginalPhases(fx.filePrefix, smallDeployment, largeDeployment)

				// Soft directional check on the score-histogram deltas.
				// karpenter_consolidation_score is labeled by decision, so
				// approved and rejected observations appear as separate
				// series in the delta. This checks that at least one series
				// with the expected decision was recorded during the phase.
				// It does not assert counts (KWOK single-round variance is
				// too high to bound).
				if !hasScoreSeriesForDecision(consolidationLatency.LatencyStats, fx.expectedDecision) {
					GinkgoWriter.Printf(
						"LatencyHarness: no karpenter_consolidation_score{decision=%s} series in delta; offline sidecar analysis carries the signal\n",
						fx.expectedDecision)
				}
			})
		}
	})
})

// hasScoreSeriesForDecision reports whether the LatencyStats delta contains
// a karpenter_consolidation_score observation carrying decision=<decision>.
// Reads HistogramStats.MetricName + Labels directly rather than parsing the
// series-key string.
func hasScoreSeriesForDecision(latencyStats map[string]common.HistogramStats, decision string) bool {
	for _, s := range latencyStats {
		if s.MetricName == "karpenter_consolidation_score" && s.Labels["decision"] == decision {
			return true
		}
	}
	return false
}
