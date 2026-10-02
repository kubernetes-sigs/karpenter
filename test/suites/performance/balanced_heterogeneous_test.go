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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
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

var _ = Describe("Performance", Label(debug.NoWatch), func() {
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
