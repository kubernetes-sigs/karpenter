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
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// buildRestrictedNodePool copies base under a prefixed name and restricts it with reqs, replacing any base
// requirement on the same keys.
func buildRestrictedNodePool(base *v1.NodePool, prefix string, reqs []v1.NodeSelectorRequirementWithMinValues) *v1.NodePool {
	np := base.DeepCopy()
	np.Name = fmt.Sprintf("%s-%s", prefix, base.Name)
	test.ReplaceRequirements(np, reqs...)
	return np
}

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Balanced Heterogeneous NodePools", func() {
		var denseReqs, sparseReqs []v1.NodeSelectorRequirementWithMinValues
		BeforeEach(func() {
			var ok bool
			if denseReqs, sparseReqs, ok = env.HeterogeneousNodePoolRequirements(); !ok {
				Skip("heterogeneous NodePool fixture needs --heterogeneous-dense-pool and --heterogeneous-sparse-pool on non-KWOK providers")
			}
		})
		It("should split load across two heterogeneous NodePools under Balanced", func() {
			By("Building two NodePools restricted to different instance shapes")
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
			densePool := buildRestrictedNodePool(nodePool, "dense", denseReqs)
			sparsePool := buildRestrictedNodePool(nodePool, "sparse", sparseReqs)
			env.ExpectCreated(nodeClass, densePool, sparsePool)

			By("Deploying dense workload targeting the dense pool")
			denseOpts := test.CreateDeploymentOptions("het-dense-app", 300, "500m", "1Gi",
				test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: densePool.Name}))
			denseDep := test.Deployment(denseOpts)

			By("Deploying sparse workload targeting the sparse pool")
			sparseOpts := test.CreateDeploymentOptions("het-sparse-app", 100, "2500m", "8Gi",
				test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: sparsePool.Name}))
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
			consolidationReport.LatencyStats = result.LatencyStats
			consolidationReport.Counters = result.Counters
			OutputPerformanceReport(consolidationReport, "balanced_heterogeneous_consolidation")

			By("Checking the scored moves belong to the two fixture NodePools")
			scoredPools := expectBalancedDecisionsMatchThreshold(result)
			Expect(scoredPools.Difference(sets.New(densePool.Name, sparsePool.Name)).UnsortedList()).To(BeEmpty(),
				"Balanced scored a move against a NodePool this fixture did not create")
		})
	})
})
