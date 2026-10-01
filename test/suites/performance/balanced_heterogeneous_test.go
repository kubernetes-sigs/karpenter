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

// Balanced consolidation across two instance-family-restricted NodePools, one
// dense and one sparse, checking that the moves Balanced scored belong to the
// pools this fixture created. KWOK only: the fixture selects on KWOK
// instance-family labels.

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
			consolidationReport.LatencyStats = result.LatencyStats
			consolidationReport.Counters = result.Counters
			OutputPerformanceReport(consolidationReport, "balanced_heterogeneous_consolidation")

			By("Checking the scored moves belong to the two fixture NodePools")
			scoredPools := expectBalancedDecisionsMatchThreshold(result)
			Expect(scoredPools.Difference(sets.New(poolC.Name, poolM.Name)).UnsortedList()).To(BeEmpty(),
				"Balanced scored a move against a NodePool this fixture did not create")
		})
	})
})
