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
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// buildFamilyRestrictedNodePool copies the suite NodePool and restricts it to a
// single KWOK instance family. The copy inherits the suite BeforeEach settings,
// including ConsolidationPolicy, so the caller pins the policy on the base
// NodePool before calling.
func buildFamilyRestrictedNodePool(base *v1.NodePool, family string) *v1.NodePool {
	np := base.DeepCopy()
	np.Name = fmt.Sprintf("%s-%s", family, base.Name)
	test.ReplaceRequirements(np, v1.NodeSelectorRequirementWithMinValues{
		Key:      v1alpha1.InstanceFamilyLabelKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{family},
	})
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

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Balanced Churn Chain", func() {
		// A 400-pod / ~40-node scale-out followed by three scale-in /
		// scale-out churn rounds, under Balanced. The RFC's 4-step max-churn
		// ceiling at k=2 predicts the counter deltas diverge from an
		// unconstrained policy by round 3. LatencyHarness spans the full churn
		// window and the sidecar JSON carries consolidation_moves_total,
		// nodeclaims_created_total, and
		// karpenter_voluntary_disruption_decision_evaluation_duration_seconds
		// deltas for offline inspection.
		//
		// There is no paired WhenEmptyOrUnderutilized arm. One was written, but
		// nothing in the tree reads the sidecars, so the second arm doubled
		// runtime to produce an artifact no comparison consumed. The
		// regression coverage is expectBalancedDecisionsMatchThreshold, which
		// needs only the Balanced arm. Reinstate a baseline arm when a
		// comparison exists to consume it.
		It("should measure churn under Balanced across three scale-in / scale-out rounds", func() {
			By("Pinning ConsolidationPolicy for this run")
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
			env.ExpectCreated(nodePool, nodeClass)

			By("Scaling out to the churn-chain fixture (400 pods)")
			opts := test.CreateDeploymentOptions("churn-chain-app", 400, "900m", "3100Mi")
			dep := test.Deployment(opts)
			env.ExpectCreated(dep)

			scaleOutReport, err := ReportScaleOutWithOutput(env,
				"Balanced Churn Chain Scale Out",
				400, 15*time.Minute,
				"balanced_churn_scale_out")
			Expect(err).ToNot(HaveOccurred())
			// TotalPods is the argument passed above, echoed back by
			// ReportScaleOut, so asserting it proves nothing;
			// EventuallyExpectHealthyPodCount inside the helper is the real pod
			// check. TotalNodes is measured, and a zero would make the
			// consolidation report below meaningless.
			initialNodes := scaleOutReport.TotalNodes
			Expect(initialNodes).To(BeNumerically(">", 0))

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
				"Balanced Churn Chain",
				400, 200, initialNodes, 20*time.Minute)
			Expect(err).ToNot(HaveOccurred())
			result, err := h.Stop()
			Expect(err).ToNot(HaveOccurred())
			emitPolicyRun(consolidationReport,
				"balanced_churn_consolidation",
				v1.ConsolidationPolicyBalanced, result)

			expectBalancedDecisionsMatchThreshold(result)
		})
	})

	Context("Balanced Heterogeneous NodePools", func() {
		// Two family-restricted NodePools ('c' and 'm' KWOK families) each
		// carry a workload at a distinct pod density profile: a dense
		// 500m/1Gi deployment on the c-pool, a sparse 2500m/8Gi deployment
		// on the m-pool. Scaling both down makes Balanced take per-pool
		// decisions (per RFC "source pool's policy governs"). The sidecar
		// carries karpenter_consolidation_moves_total{nodepool}, per-pool
		// disruption timing, and karpenter_nodeclaims_created_total.
		//
		// Pods select their pool with karpenter.sh/nodepool, which
		// nodeclaimtemplate.go already stamps on provisioned nodes. An earlier
		// revision used a custom perf.karpenter.sh/pool label; the NodePool
		// CRD's CEL rule rejects any label under a karpenter.sh subdomain, so
		// the NodePools failed admission and this context never ran.
		BeforeEach(func() {
			if !env.IsDefaultNodeClassKWOK() {
				Skip("heterogeneous NodePool fixture uses KWOK-only instance-family labels")
			}
		})
		It("should split load across two heterogeneous NodePools under Balanced", func() {
			By("Building two family-restricted NodePools")
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
			poolC := buildFamilyRestrictedNodePool(nodePool, "c")
			poolM := buildFamilyRestrictedNodePool(nodePool, "m")
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
				"Balanced Heterogeneous Scale Out",
				400, 15*time.Minute,
				"balanced_heterogeneous_scale_out")
			Expect(err).ToNot(HaveOccurred())
			initialNodes := scaleOutReport.TotalNodes
			Expect(initialNodes).To(BeNumerically(">", 0))

			By("Starting LatencyHarness for the consolidation window")
			h, err := common.StartLatencyHarness(env)
			Expect(err).ToNot(HaveOccurred())

			By("Scaling both deployments down to trigger cross-pool consolidation")
			denseDep.Spec.Replicas = lo.ToPtr(int32(180))
			sparseDep.Spec.Replicas = lo.ToPtr(int32(60))
			env.ExpectUpdated(denseDep, sparseDep)

			By("Recording the consolidation phase")
			consolidationReport, err := ReportConsolidation(env,
				"Balanced Heterogeneous",
				400, 240, initialNodes, 25*time.Minute)
			Expect(err).ToNot(HaveOccurred())

			By("Capturing LatencyHarness result at end of consolidation")
			result, err := h.Stop()
			Expect(err).ToNot(HaveOccurred())
			emitPolicyRun(consolidationReport,
				"balanced_heterogeneous_consolidation",
				v1.ConsolidationPolicyBalanced, result)

			By("Checking the scored moves belong to the two fixture NodePools")
			// This context exists to exercise per-pool decisions, so the
			// nodepool label is the point. Asserting the observed pools are a
			// subset of the two created catches scoring attributed to a pool
			// the fixture never made. It deliberately does not require both
			// pools to have scored: whether the m-pool consolidates at all
			// depends on how KWOK packs 60 sparse pods, and requiring it would
			// make a 25-minute spec flaky.
			scoredPools := expectBalancedDecisionsMatchThreshold(result)
			Expect(scoredPools.Difference(sets.New(poolC.Name, poolM.Name)).UnsortedList()).To(BeEmpty(),
				"Balanced scored a move against a NodePool this fixture did not create")
		})
	})
})
