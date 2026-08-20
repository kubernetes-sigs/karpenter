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

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

const (
	// manyNodePoolsPodLabelKey pins pods to a specific NodePool via nodeSelector
	// and the pool via a matching NodePool requirement. The taint of the same
	// key blocks pods without a matching toleration from landing on the pool's
	// nodes. Together these give per-pool workload isolation so consolidation
	// cannot merge across pools and defeat the reconciler-scan measurement.
	manyNodePoolsPodLabelKey = "mnp-pool"

	// manyNodePoolsPodCPU and manyNodePoolsPodMemory are intentionally small so
	// bin-packing is not the constraint. The signal we stress is per-pool
	// scheduler and disruption-loop cost, not resource fit.
	manyNodePoolsPodCPU    = "100m"
	manyNodePoolsPodMemory = "128Mi"

	// manyNodePoolsPodsPerPool is the baseline replica count per NodePool for
	// the initial scale-out and the second (re)scale-out phase. Scale-in halves
	// this to manyNodePoolsScaleInPodsPerPool.
	manyNodePoolsPodsPerPool        = 2
	manyNodePoolsScaleInPodsPerPool = 1

	// manyNodePoolsWarmUpDuration lets the NodePool subcontrollers (hash,
	// counter, readiness, registrationhealth) reach steady state before the
	// first workload lands. At the largest sweep size the first-touch reconcile
	// churn is on the order of tens of seconds; isolating it keeps the
	// scale-out measurement clean.
	manyNodePoolsWarmUpDuration = 60 * time.Second
)

// manyNodePoolsFamilies is the KWOK instance-family set. parseFamilyFromType in
// kwok/cloudprovider/helpers.go splits the instance-type name on the first
// [.-] and takes the first token. Instance-type names in kwok/cloudprovider/
// instance_types.json use format "<family>-<size>x-<arch>-<os>", producing
// families c, m, s (verified via a scan of instance_types.json at scoping
// time).
var manyNodePoolsFamilies = []string{"c", "m", "s"}

// manyNodePoolsSizes is the KWOK instance-size set as it appears on the node
// label karpenter.kwok.sh/instance-size. parseSizeFromType falls back to the
// CPU-count string when the AWS name regex does not match, so the label
// carries values like "1", "2", "4", "8", "16". The suite-wide BeforeEach in
// suite_test.go replaces the instance-size requirement with Lt "32" for KWOK,
// which admits only these five sizes; keep the set aligned so per-pool
// requirements do not intersect to empty.
var manyNodePoolsSizes = []string{"1", "2", "4", "8", "16"}

// buildManyNodePool returns a NodePool derived from the suite BeforeEach's
// shared template. Distinctness is enforced three ways: (1) a per-pool
// InstanceFamily requirement combined with an InstanceSize requirement so the
// scheduler cannot short-circuit its per-pool instance-type walk; (2) a
// per-pool label on the template so cross-pool consolidation cannot find
// interchangeable candidates; (3) a per-pool taint that blocks pods without a
// matching toleration from crossing pool boundaries. The three constraints
// are redundant on purpose so a subtle mismatch in one path does not silently
// weaken the isolation the perf test depends on.
func buildManyNodePool(template *v1.NodePool, index int) *v1.NodePool {
	np := template.DeepCopy()
	name := fmt.Sprintf("mnp-%03d", index)
	np.Name = name
	np.ResourceVersion = ""

	family := manyNodePoolsFamilies[index%len(manyNodePoolsFamilies)]
	size := manyNodePoolsSizes[(index/len(manyNodePoolsFamilies))%len(manyNodePoolsSizes)]

	test.ReplaceRequirements(np,
		v1.NodeSelectorRequirementWithMinValues{
			Key:      v1alpha1.InstanceFamilyLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{family},
		},
		v1.NodeSelectorRequirementWithMinValues{
			Key:      v1alpha1.InstanceSizeLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{size},
		},
		v1.NodeSelectorRequirementWithMinValues{
			Key:      manyNodePoolsPodLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{name},
		},
	)

	if np.Spec.Template.Labels == nil {
		np.Spec.Template.Labels = map[string]string{}
	}
	np.Spec.Template.Labels[manyNodePoolsPodLabelKey] = name

	np.Spec.Template.Spec.Taints = append(np.Spec.Template.Spec.Taints, corev1.Taint{
		Key:    manyNodePoolsPodLabelKey,
		Value:  name,
		Effect: corev1.TaintEffectNoSchedule,
	})

	return np
}

// buildManyNodePoolDeployment returns a Deployment pinned to a single
// NodePool. The nodeSelector routes scheduling and the toleration matches
// the pool's taint. Pod resources stay small (100m / 128Mi) so bin-packing
// is not the constraint; the signal we care about is per-pool reconciler
// cost.
func buildManyNodePoolDeployment(poolName string, replicas int32) *appsv1.Deployment {
	opts := test.CreateDeploymentOptions(
		fmt.Sprintf("%s-dep", poolName),
		replicas,
		manyNodePoolsPodCPU,
		manyNodePoolsPodMemory,
		test.WithNodeSelector(map[string]string{manyNodePoolsPodLabelKey: poolName}),
		test.WithTolerations([]corev1.Toleration{{
			Key:      manyNodePoolsPodLabelKey,
			Operator: corev1.TolerationOpEqual,
			Value:    poolName,
			Effect:   corev1.TaintEffectNoSchedule,
		}}),
	)
	return test.Deployment(opts)
}

// startPhaseLatencyHarness wraps the harness Start / Stop pair with the
// sidecar-write posture used by the Balanced perf specs on the same LatencyHarness
// substrate. Callers Stop() the harness after the phase's Report* returns and
// invoke writeLatencySidecar to emit the paired JSON when OUTPUT_DIR is set.
func startPhaseLatencyHarness() *common.LatencyHarness {
	harness, err := common.StartLatencyHarness(env)
	Expect(err).ToNot(HaveOccurred())
	return harness
}

// writeManyNodePoolsLatencySidecar emits a paired latency-companion JSON to
// OUTPUT_DIR when set, matching the shape performance-suite peers use. A
// write error is logged and swallowed: the primary report is already on
// disk, and downstream analysis treats the sidecar as best-effort. The
// consolidation-policy field records the effective policy for the phase so
// the sidecar carries the run-time value rather than a compile-time
// constant.
func writeManyNodePoolsLatencySidecar(testName, filePrefix string, policy v1.ConsolidationPolicy, result *common.LatencyResult) {
	err := common.WriteLatencySidecar(os.Getenv("OUTPUT_DIR"), filePrefix, common.LatencySidecar{
		TestName:            testName,
		ConsolidationPolicy: string(policy),
		Timestamp:           time.Now(),
		LatencyStats:        result.LatencyStats,
		Counters:            result.Counters,
	})
	if err != nil {
		GinkgoWriter.Printf("LatencyHarness: %v\n", err)
	}
}

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Many NodePools", func() {
		// The DescribeTable sweeps NodePool counts to characterize the
		// per-pool reconciler-scan cost as an emergent scaling curve
		// rather than a single 500-pool data point. Assertions are
		// deliberately soft: verify pod counts and error-free execution;
		// let the emitted PerformanceReport JSON and paired
		// LatencySidecar carry the quantitative signal for offline
		// analysis. Threshold-based hard bounds land in a follow-up once
		// the curve is characterized on the fork's CI.
		DescribeTable("scaling curve baseline scale-out, scale-in, second scale-out",
			func(nodePoolCount int) {
				totalInitialPods := nodePoolCount * manyNodePoolsPodsPerPool
				totalScaleInPods := nodePoolCount * manyNodePoolsScaleInPodsPerPool
				policy := nodePool.Spec.Disruption.ConsolidationPolicy
				filePrefixBase := fmt.Sprintf("many_nodepools_%d", nodePoolCount)
				testNameBase := fmt.Sprintf("Many NodePools %d", nodePoolCount)

				By(fmt.Sprintf("Creating %d distinct NodePools plus one shared NodeClass", nodePoolCount))
				env.ExpectCreated(nodeClass)
				pools := make([]*v1.NodePool, nodePoolCount)
				for i := 0; i < nodePoolCount; i++ {
					pools[i] = buildManyNodePool(nodePool, i)
					env.ExpectCreated(pools[i])
				}

				By(fmt.Sprintf("Waiting %s for NodePool subcontrollers to reach steady state", manyNodePoolsWarmUpDuration))
				time.Sleep(manyNodePoolsWarmUpDuration)

				// Phase 1: initial scale-out 0 -> 2 pods per NodePool.
				scaleOutPrefix := fmt.Sprintf("%s_scale_out", filePrefixBase)
				scaleOutName := fmt.Sprintf("%s Scale Out", testNameBase)
				By(fmt.Sprintf("Phase 1 scale-out: 0 -> %d pods per NodePool (%d pods total)", manyNodePoolsPodsPerPool, totalInitialPods))

				deployments := make([]*appsv1.Deployment, nodePoolCount)
				for i := 0; i < nodePoolCount; i++ {
					deployments[i] = buildManyNodePoolDeployment(pools[i].Name, int32(manyNodePoolsPodsPerPool))
					env.ExpectCreated(deployments[i])
				}

				scaleOutHarness := startPhaseLatencyHarness()
				scaleOutReport, err := ReportScaleOutWithOutput(env, scaleOutName, totalInitialPods, 30*time.Minute, scaleOutPrefix)
				Expect(err).ToNot(HaveOccurred(), "Phase 1 scale-out should complete without error")
				scaleOutLatency, err := scaleOutHarness.Stop()
				Expect(err).ToNot(HaveOccurred())
				writeManyNodePoolsLatencySidecar(scaleOutName, scaleOutPrefix, policy, scaleOutLatency)
				Expect(scaleOutReport.TestType).To(Equal("scale-out"))
				Expect(scaleOutReport.TotalPods).To(Equal(totalInitialPods))
				initialNodes := scaleOutReport.TotalNodes

				// Phase 2: scale-in 2 -> 1 pod per NodePool.
				consolidationPrefix := fmt.Sprintf("%s_consolidation", filePrefixBase)
				consolidationName := fmt.Sprintf("%s Consolidation", testNameBase)
				By(fmt.Sprintf("Phase 2 scale-in: %d -> %d pods per NodePool (%d pods total)", manyNodePoolsPodsPerPool, manyNodePoolsScaleInPodsPerPool, totalScaleInPods))

				for i := 0; i < nodePoolCount; i++ {
					deployments[i].Spec.Replicas = new(int32(manyNodePoolsScaleInPodsPerPool))
					env.ExpectUpdated(deployments[i])
				}

				consolidationHarness := startPhaseLatencyHarness()
				consolidationReport, err := ReportConsolidationWithOutput(env, consolidationName, totalInitialPods, totalScaleInPods, initialNodes, 30*time.Minute, consolidationPrefix)
				Expect(err).ToNot(HaveOccurred(), "Phase 2 consolidation should complete without error")
				consolidationLatency, err := consolidationHarness.Stop()
				Expect(err).ToNot(HaveOccurred())
				writeManyNodePoolsLatencySidecar(consolidationName, consolidationPrefix, policy, consolidationLatency)
				Expect(consolidationReport.TestType).To(Equal("consolidation"))
				Expect(consolidationReport.TotalPods).To(Equal(totalScaleInPods))

				// Phase 3: second scale-out 1 -> 2 pods per NodePool. This
				// measures a warm-cluster provisioning fan-out (informer
				// caches populated, NodePool subcontrollers past their
				// first-touch churn) as a control against Phase 1, which
				// includes cold-cluster churn.
				scaleOutRepeatPrefix := fmt.Sprintf("%s_scale_out_repeat", filePrefixBase)
				scaleOutRepeatName := fmt.Sprintf("%s Scale Out Repeat", testNameBase)
				By(fmt.Sprintf("Phase 3 scale-out repeat: %d -> %d pods per NodePool (%d pods total)", manyNodePoolsScaleInPodsPerPool, manyNodePoolsPodsPerPool, totalInitialPods))

				for i := 0; i < nodePoolCount; i++ {
					deployments[i].Spec.Replicas = new(int32(manyNodePoolsPodsPerPool))
					env.ExpectUpdated(deployments[i])
				}

				scaleOutRepeatHarness := startPhaseLatencyHarness()
				scaleOutRepeatReport, err := ReportScaleOutWithOutput(env, scaleOutRepeatName, totalInitialPods, 30*time.Minute, scaleOutRepeatPrefix)
				Expect(err).ToNot(HaveOccurred(), "Phase 3 scale-out repeat should complete without error")
				scaleOutRepeatLatency, err := scaleOutRepeatHarness.Stop()
				Expect(err).ToNot(HaveOccurred())
				writeManyNodePoolsLatencySidecar(scaleOutRepeatName, scaleOutRepeatPrefix, policy, scaleOutRepeatLatency)
				Expect(scaleOutRepeatReport.TestType).To(Equal("scale-out"))
				Expect(scaleOutRepeatReport.TotalPods).To(Equal(totalInitialPods))
			},
			Entry("50 NodePools", 50),
			Entry("100 NodePools", 100),
			Entry("250 NodePools", 250),
			Entry("500 NodePools", 500),
		)
	})
})
