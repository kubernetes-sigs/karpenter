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
	Context("Host Name Spreading Deployment XL", func() {
		It("should efficiently scale two deployments with host name topology spreading at XL scale", func() {
			By("Setting up NodePool and NodeClass for the XL test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: XL SCALE-OUT TEST WITH HOSTNAME SPREADING ==========
			By("Creating XL deployments with hostname spreading")

			// Create deployment options using templates - XL scale (2000 pods total)
			smallOpts := test.CreateDeploymentOptions("small-resource-app", 1000, "950m", "3900Mi",
				test.WithHostnameSpread())
			largeOpts := test.CreateDeploymentOptions("large-resource-app", 1000, "3800m", "31Gi")

			// Create deployments
			smallDeployment := test.Deployment(smallOpts)
			largeDeployment := test.Deployment(largeOpts)

			env.ExpectCreated(smallDeployment, largeDeployment)

			By("Monitoring XL scale-out performance with hostname spreading (2000 pods)")
			scaleOutReport, err := ReportScaleOutWithOutput(env, "XL Host Name Spreading Performance Test", 2000, 20*time.Minute, "hostname_spread_xl_scale_out")
			Expect(err).ToNot(HaveOccurred(), "XL scale-out should execute successfully")

			By("Validating XL scale-out performance with hostname spreading")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(2000), "Should have 2000 total pods")

			// XL Performance assertions - hostname spreading at scale may require many more nodes
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 10*time.Minute),
				"Total XL scale-out time should be less than 7 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 1200),
				"Should not require more than 2000 nodes for 2000 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.55),
				"Average CPU utilization should be greater than 55%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.7),
				"Average memory utilization should be greater than 70%")

			// ========== PHASE 2: XL CONSOLIDATION TEST ==========
			By("Scaling down XL deployments to trigger consolidation")
			initialNodes := scaleOutReport.TotalNodes

			// Scale down both deployments
			smallDeployment.Spec.Replicas = lo.ToPtr(int32(700))
			largeDeployment.Spec.Replicas = lo.ToPtr(int32(700))
			env.ExpectUpdated(smallDeployment, largeDeployment)

			By("Monitoring XL consolidation performance")
			consolidationReport, err := ReportConsolidationWithOutput(env, "XL Hostname Spread Consolidation Test", 2000, 1400, initialNodes, 25*time.Minute, "hostname_spread_xl_consolidation")
			Expect(err).ToNot(HaveOccurred(), "XL consolidation should execute successfully")

			By("Validating XL consolidation performance")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(1400), "Should have 1400 total pods after scale-in")
			Expect(consolidationReport.PodsNetChange).To(Equal(-600), "Should have net reduction of 600 pods")

			// XL Consolidation assertions
			Expect(consolidationReport.NodesNetChange).To(BeNumerically("<", 0),
				"Node count should decrease after consolidation")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 10*time.Minute),
				"XL consolidation should complete within 10 minutes")
			Expect(consolidationReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.55),
				"Average CPU utilization should be greater than 55%")
			Expect(consolidationReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.7),
				"Average memory utilization should be greater than 70%")

		})
	})
})
