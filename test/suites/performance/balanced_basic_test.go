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

// Balanced consolidation over the basic two-deployment fixture: scale out to
// 1000 pods, cut both deployments to 350 replicas, and measure the consolidation
// phase with ConsolidationPolicyBalanced set on the NodePool.

package performance

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Balanced Basic Fixture", func() {
		It("should consolidate the basic fixture under Balanced", func() {
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
			smallDeployment := test.Deployment(test.CreateDeploymentOptions("small-resource-app", 500, "900m", "3100Mi"))
			largeDeployment := test.Deployment(test.CreateDeploymentOptions("large-resource-app", 500, "3500m", "28Gi"))
			env.ExpectCreated(nodePool, nodeClass, smallDeployment, largeDeployment)

			scaleOutReport, err := ReportScaleOutWithOutput(env, "Balanced Basic Scale Out", 1000, 15*time.Minute, "balanced_basic_scale_out")
			Expect(err).ToNot(HaveOccurred())
			Expect(scaleOutReport.TotalNodes).To(BeNumerically(">", 0))

			h, err := common.StartLatencyHarness(env)
			Expect(err).ToNot(HaveOccurred())

			smallDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			largeDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			env.ExpectUpdated(smallDeployment, largeDeployment)

			consolidationReport, err := ReportConsolidation(env, "Balanced Basic Consolidation", 1000, 700, scaleOutReport.TotalNodes, 20*time.Minute)
			Expect(err).ToNot(HaveOccurred())
			result, err := h.Stop()
			Expect(err).ToNot(HaveOccurred())
			consolidationReport.LatencyStats = result.LatencyStats
			consolidationReport.Counters = result.Counters
			OutputPerformanceReport(consolidationReport, "balanced_basic_consolidation")

			expectBalancedDecisionsMatchThreshold(result)
		})
	})
})
