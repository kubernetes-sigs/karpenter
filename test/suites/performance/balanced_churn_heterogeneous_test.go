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
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// balancedPolicies enumerates the paired baseline vs Balanced iteration used
// by both spec groups. Baseline first, then Balanced; per-spec AfterEach
// makes the ordering independent of correctness.
var balancedPolicies = []v1.ConsolidationPolicy{
	v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
	v1.ConsolidationPolicyBalanced,
}

// policyPrefix maps a ConsolidationPolicy to the short filePrefix segment
// used in artifact filenames. WhenEmptyOrUnderutilized is the reference
// baseline; Balanced is the arm under test.
func policyPrefix(p v1.ConsolidationPolicy) string {
	if p == v1.ConsolidationPolicyBalanced {
		return "balanced"
	}
	return "baseline"
}

// buildFamilyRestrictedNodePool copies the suite NodePool and restricts it to a
// single KWOK instance family.
func buildFamilyRestrictedNodePool(base *v1.NodePool, family string, policy v1.ConsolidationPolicy) *v1.NodePool {
	np := base.DeepCopy()
	np.Name = fmt.Sprintf("%s-%s", family, base.Name)
	test.ReplaceRequirements(np, v1.NodeSelectorRequirementWithMinValues{
		Key:      v1alpha1.InstanceFamilyLabelKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{family},
	})
	np.Spec.Disruption.ConsolidationPolicy = policy
	return np
}

// scaleAndSettle updates the deployment to targetReplicas, waits for pods to
// reach that count, then sleeps two consolidateAfter cycles so the
// disruption controller has time to act before the next round.
func scaleAndSettle(env *common.Environment, dep *appsv1.Deployment, targetReplicas int32, timeout time.Duration) {
	dep.Spec.Replicas = lo.ToPtr(targetReplicas)
	env.ExpectUpdated(dep)
	sel := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	env.EventuallyExpectHealthyPodCountWithTimeout(timeout, sel, int(targetReplicas))
	time.Sleep(2 * lo.FromPtr(nodePool.Spec.Disruption.ConsolidateAfter.Duration))
}

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
	if err := common.WriteLatencySidecar(os.Getenv("OUTPUT_DIR"), filePrefix, sc); err != nil {
		GinkgoWriter.Printf("LatencyHarness: %v\n", err)
	}
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
// below the 0.5 bucket.
func expectBalancedDecisionsMatchThreshold(result *common.LatencyResult) {
	threshold := 1.0 / float64(v1.BalancedK)
	scored := uint64(0)
	for key, s := range result.LatencyStats {
		if s.MetricName != "karpenter_consolidation_score" || s.Count == 0 || s.Labels["policy"] != string(v1.ConsolidationPolicyBalanced) {
			continue
		}
		scored += s.Count
		switch s.Labels["decision"] {
		case "approved":
			Expect(s.Min).To(BeNumerically(">=", scoreBucketBelowThreshold), "%s: approved a move scoring below the %.2f threshold", key, threshold)
		case "rejected":
			Expect(s.Max).To(BeNumerically("<=", threshold), "%s: rejected a move scoring above the %.2f threshold", key, threshold)
		}
	}
	Expect(scored).To(BeNumerically(">", 0), "Balanced recorded no scored consolidation moves")
}

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Balanced Churn Chain", func() {
		// Each It runs one policy over a 400-pod / ~40-node scale-out then
		// three scale-in / scale-out churn rounds. The RFC's 4-step
		// max-churn ceiling at k=2 predicts Balanced's counter deltas
		// diverge from baseline's by round 3. Comparison is offline: the
		// paired PerformanceReport plus latency sidecar JSONs carry
		// consolidation_moves_total, nodeclaims_created_total, and
		// karpenter_voluntary_disruption_decision_evaluation_duration_seconds
		// deltas per policy. LatencyHarness spans the full churn window.
		for _, policy := range balancedPolicies {
			prefix := policyPrefix(policy)
			It(fmt.Sprintf("should measure churn under %s across three scale-in / scale-out rounds", policy), func() {
				By("Pinning ConsolidationPolicy for this run")
				nodePool.Spec.Disruption.ConsolidationPolicy = policy
				env.ExpectCreated(nodePool, nodeClass)

				By("Scaling out to the churn-chain fixture (400 pods)")
				opts := test.CreateDeploymentOptions("churn-chain-app", 400, "900m", "3100Mi")
				dep := test.Deployment(opts)
				env.ExpectCreated(dep)

				scaleOutReport, err := ReportScaleOutWithOutput(env,
					fmt.Sprintf("Balanced Churn Chain %s Scale Out", policy),
					400, 15*time.Minute,
					fmt.Sprintf("balanced_churn_%s_scale_out", prefix))
				Expect(err).ToNot(HaveOccurred())
				Expect(scaleOutReport.TotalPods).To(Equal(400))
				initialNodes := scaleOutReport.TotalNodes

				By("Starting LatencyHarness for the churn window")
				h, err := common.StartLatencyHarness(env)
				Expect(err).ToNot(HaveOccurred())

				By("Round 1: scale in to 200 pods")
				scaleAndSettle(env, dep, 200, 10*time.Minute)
				By("Round 2: scale back out to 400 pods")
				scaleAndSettle(env, dep, 400, 10*time.Minute)
				By("Round 3: scale in to 200 pods")
				scaleAndSettle(env, dep, 200, 10*time.Minute)

				By("Waiting for consolidation to settle after the last round")
				consolidationReport, err := ReportConsolidation(env,
					fmt.Sprintf("Balanced Churn Chain %s", policy),
					400, 200, initialNodes, 20*time.Minute)
				Expect(err).ToNot(HaveOccurred())
				result, err := h.Stop()
				Expect(err).ToNot(HaveOccurred())
				emitPolicyRun(consolidationReport,
					fmt.Sprintf("balanced_churn_%s_consolidation", prefix),
					policy, result)
				if policy == v1.ConsolidationPolicyBalanced {
					expectBalancedDecisionsMatchThreshold(result)
				}

			})
		}
	})

	Context("Balanced Heterogeneous NodePools", func() {
		// Two family-restricted NodePools ('c' and 'm' KWOK families) each
		// carry a workload at a distinct pod density profile: a dense
		// 500m/1Gi deployment on the c-pool, a sparse 2500m/8Gi deployment
		// on the m-pool. Scaling both down triggers Balanced to make
		// per-pool decisions (per RFC "source pool's policy governs") vs
		// baseline which accepts any positive-savings move. Comparison is
		// offline: paired PerformanceReport plus latency sidecar JSONs
		// carry karpenter_consolidation_moves_total{nodepool}, per-pool
		// disruption timing, and karpenter_nodeclaims_created_total per
		// policy.
		BeforeEach(func() {
			if !env.IsDefaultNodeClassKWOK() {
				Skip("heterogeneous NodePool fixture uses KWOK-only instance-family labels")
			}
		})
		for _, policy := range balancedPolicies {
			prefix := policyPrefix(policy)
			It(fmt.Sprintf("should split load across two heterogeneous NodePools under %s", policy), func() {
				By("Building two family-restricted NodePools")
				poolC := buildFamilyRestrictedNodePool(nodePool, "c", policy)
				poolM := buildFamilyRestrictedNodePool(nodePool, "m", policy)
				env.ExpectCreated(nodeClass, poolC, poolM)

				By("Deploying dense workload targeting the c-family pool")
				denseOpts := test.CreateDeploymentOptions("het-dense-app", 300, "500m", "1Gi",
					test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: poolC.Name}))
				denseDep := test.Deployment(denseOpts)

				By("Deploying sparse workload targeting the m-family pool")
				sparseOpts := test.CreateDeploymentOptions("het-sparse-app", 100, "2500m", "8Gi",
					test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: poolM.Name}))
				sparseDep := test.Deployment(sparseOpts)

				env.ExpectCreated(denseDep, sparseDep)

				scaleOutReport, err := ReportScaleOutWithOutput(env,
					fmt.Sprintf("Balanced Heterogeneous %s Scale Out", policy),
					400, 15*time.Minute,
					fmt.Sprintf("balanced_heterogeneous_%s_scale_out", prefix))
				Expect(err).ToNot(HaveOccurred())
				Expect(scaleOutReport.TotalPods).To(Equal(400))
				initialNodes := scaleOutReport.TotalNodes

				By("Starting LatencyHarness for the consolidation window")
				h, err := common.StartLatencyHarness(env)
				Expect(err).ToNot(HaveOccurred())

				By("Scaling both deployments down to trigger cross-pool consolidation")
				denseReplicas := int32(180)
				sparseReplicas := int32(60)
				denseDep.Spec.Replicas = &denseReplicas
				sparseDep.Spec.Replicas = &sparseReplicas
				env.ExpectUpdated(denseDep, sparseDep)

				By("Recording the consolidation phase")
				consolidationReport, err := ReportConsolidation(env,
					fmt.Sprintf("Balanced Heterogeneous %s", policy),
					400, 240, initialNodes, 25*time.Minute)
				Expect(err).ToNot(HaveOccurred())

				By("Capturing LatencyHarness result at end of consolidation")
				result, err := h.Stop()
				Expect(err).ToNot(HaveOccurred())
				emitPolicyRun(consolidationReport,
					fmt.Sprintf("balanced_heterogeneous_%s_consolidation", prefix),
					policy, result)

				if policy == v1.ConsolidationPolicyBalanced {
					expectBalancedDecisionsMatchThreshold(result)
				}
			})
		}
	})
})
