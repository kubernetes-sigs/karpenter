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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// balancedPolicies enumerates the paired baseline vs Balanced iteration used
// by both spec groups. Baseline runs first so the second run starts from an
// AfterEach-clean state, keeping diff analysis stable.
var balancedPolicies = []v1.ConsolidationPolicy{
	v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
	v1.ConsolidationPolicyBalanced,
}

// suiteConsolidateAfter mirrors the value pinned in suite_test.go BeforeEach.
// Update in lockstep if the suite-wide default changes.
const suiteConsolidateAfter = 30 * time.Second

// scaleAndSettleWaitFactor sets the sleep after scale-out at 2× ConsolidateAfter
// to give the consolidation controller two full evaluation cycles before we sample metrics.
const scaleAndSettleWaitFactor = 2

// policyPrefix maps a ConsolidationPolicy to the short filePrefix segment
// used in artifact filenames. WhenEmptyOrUnderutilized is the reference
// baseline; Balanced is the arm under test.
func policyPrefix(p v1.ConsolidationPolicy) string {
	if p == v1.ConsolidationPolicyBalanced {
		return "balanced"
	}
	return "baseline"
}

// buildFamilyRestrictedNodePool constructs a NodePool restricted to a single
// KWOK instance family, carrying the standard suite requirements (linux
// pinned via defaultNodePool, on-demand pinned via defaultNodePool, instance
// size clamped < 32 to match the suite-wide default). The nodePool is a
// clone of env.DefaultNodePool with the family requirement layered on.
func buildFamilyRestrictedNodePool(env *common.Environment, nodeClass *unstructured.Unstructured, family string, policy v1.ConsolidationPolicy) *v1.NodePool {
	np := env.DefaultNodePool(nodeClass)
	np.Name = fmt.Sprintf("%s-%s", family, np.Name)
	np.Spec.Template.Labels["perf.karpenter.sh/pool"] = fmt.Sprintf("%s-pool", family)
	test.ReplaceRequirements(np,
		v1.NodeSelectorRequirementWithMinValues{
			Key:      v1alpha1.InstanceFamilyLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{family},
		},
		v1.NodeSelectorRequirementWithMinValues{
			Key:      v1alpha1.InstanceSizeLabelKey,
			Operator: corev1.NodeSelectorOpLt,
			Values:   []string{"32"},
		},
	)
	np.Spec.Limits = v1.Limits{}
	np.Spec.Disruption.ConsolidationPolicy = policy
	np.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("30s")
	np.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
	return np
}

// scaleAndSettle updates the deployment to targetReplicas, waits for pods to
// reach that count, then sleeps two consolidateAfter cycles so the
// disruption controller has time to act before the round-end capture.
func scaleAndSettle(env *common.Environment, dep *appsv1.Deployment, targetReplicas int32, timeout time.Duration) {
	replicas := targetReplicas
	dep.Spec.Replicas = &replicas
	env.ExpectUpdated(dep)
	sel := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	env.EventuallyExpectHealthyPodCountWithTimeout(timeout, sel, int(targetReplicas))
	time.Sleep(scaleAndSettleWaitFactor * suiteConsolidateAfter)
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

				By("Capturing LatencyHarness result at end of churn window")
				result, err := h.Stop()
				Expect(err).ToNot(HaveOccurred())

				By("Emitting the consolidation report and latency sidecar")
				consolidationReport, err := ReportConsolidation(env,
					fmt.Sprintf("Balanced Churn Chain %s", policy),
					400, 200, initialNodes, 20*time.Minute)
				Expect(err).ToNot(HaveOccurred())
				emitPolicyRun(consolidationReport,
					fmt.Sprintf("balanced_churn_%s_consolidation", prefix),
					policy, result)

				// Soft check: harness must have observed at least one
				// scrape delta. Comparison across policies is offline on
				// the paired JSON artifacts; hard bounds are not asserted
				// because KWOK timing variance blurs per-round counts.
				Expect(result.Counters).ToNot(BeNil())
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
				poolC := buildFamilyRestrictedNodePool(env, nodeClass, "c", policy)
				poolM := buildFamilyRestrictedNodePool(env, nodeClass, "m", policy)
				env.ExpectCreated(nodeClass, poolC, poolM)

				By("Deploying dense workload targeting the c-family pool")
				denseOpts := test.CreateDeploymentOptions("het-dense-app", 300, "500m", "1Gi",
					test.WithNodeSelector(map[string]string{"perf.karpenter.sh/pool": "c-pool"}))
				denseDep := test.Deployment(denseOpts)

				By("Deploying sparse workload targeting the m-family pool")
				sparseOpts := test.CreateDeploymentOptions("het-sparse-app", 100, "2500m", "8Gi",
					test.WithNodeSelector(map[string]string{"perf.karpenter.sh/pool": "m-pool"}))
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

				Expect(consolidationReport.TotalPods).To(Equal(240))
				Expect(result.Counters).ToNot(BeNil())
			})
		}
	})
})
