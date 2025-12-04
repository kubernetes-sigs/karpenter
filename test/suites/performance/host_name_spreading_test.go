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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Performance", func() {
	Context("Host Name Spreading Deployment Reg", func() {
		It("should efficiently scale two deployments with host name topology spreading", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: SCALE-OUT TEST WITH HOSTNAME SPREADING ==========
			By("Creating deployments with hostname spreading")

			// Create deployment options using templates
			smallOpts := test.CreateDeploymentOptions("small-resource-app", 500, "950m", "3900Mi",
				test.WithHostnameSpread())
			largeOpts := test.CreateDeploymentOptions("large-resource-app", 500, "3800m", "31Gi")

			// Create deployments
			smallDeployment := test.Deployment(smallOpts)
			largeDeployment := test.Deployment(largeOpts)

			env.ExpectCreated(smallDeployment, largeDeployment)

			By("Monitoring scale-out performance with hostname spreading (1000 pods)")
			scaleOutReport, err := ReportScaleOutWithOutput(env, "Host Name Spreading Performance Test", 1000, 15*time.Minute, "hostname_spread_scale_out")
			Expect(err).ToNot(HaveOccurred(), "Scale-out should execute successfully")

			By("Validating scale-out performance with hostname spreading")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(1000), "Should have 1000 total pods")

			// Performance assertions - hostname spreading may require more nodes
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 5*time.Minute),
				"Total scale-out time should be less than 10 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 650),
				"Should not require more than 1000 nodes for 1000 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.55),
				"Average CPU utilization should be greater than 55%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.75),
				"Average memory utilization should be greater than 75%")

			// ========== PHASE 2: CONSOLIDATION TEST ==========
			By("Scaling down deployments to trigger consolidation")
			initialNodes := scaleOutReport.TotalNodes

			// Scale down both deployments
			smallDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			largeDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			env.ExpectUpdated(smallDeployment, largeDeployment)

			By("Monitoring consolidation performance")
			consolidationReport, err := ReportConsolidationWithOutput(env, "Hostname Spread Consolidation Test", 1000, 700, initialNodes, 20*time.Minute, "hostname_spread_consolidation")
			Expect(err).ToNot(HaveOccurred(), "Consolidation should execute successfully")

			By("Validating consolidation performance")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(700), "Should have 700 total pods after scale-in")
			Expect(consolidationReport.PodsNetChange).To(Equal(-300), "Should have net reduction of 300 pods")

			// Consolidation assertions
			Expect(consolidationReport.NodesNetChange).To(BeNumerically("<", 0),
				"Node count should decrease after consolidation")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 10*time.Minute),
				"Consolidation should complete within 10 minutes")
			Expect(consolidationReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.55),
				"Average CPU utilization should be greater than 55%")
			Expect(consolidationReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.75),
				"Average memory utilization should be greater than 75%")

		})
	})
})
