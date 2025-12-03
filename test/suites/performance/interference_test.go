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
	Context("Host Name Spreading Deployment Interference", func() {
		It("should efficiently scale two deployments with host name topology spreading", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: SCALE-OUT TEST WITH HOSTNAME SPREADING ==========
			By("Creating deployments with hostname spreading")

			// Create deployment options using templates
			smallOpts := test.CreateDeploymentOptions("small-resource-app", 500, "950m", "3900Mi",
				test.WithHostnameSpread())
			largeOpts := test.CreateDeploymentOptions("large-resource-app", 500, "3800m", "31Gi", test.WithHostnameSpread())

			// Create deployments
			smallDeployment := test.Deployment(smallOpts)

			env.ExpectCreated(smallDeployment)

			By("Monitoring scale-out performance with hostname spreading (1000 pods)")
			scaleOutReport, err := ReportScaleOutWithOutput(env, "Host Name Spreading Performance Test", 1000, 15*time.Minute, "hostname_spread_scale_out_small")
			Expect(err).ToNot(HaveOccurred(), "Scale-out should execute successfully")

			By("Validating scale-out performance with hostname spreading")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(500), "Should have 1000 total pods")

			// Performance assertions - hostname spreading may require more nodes
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 5*time.Minute),
				"Total scale-out time should be less than 10 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 650),
				"Should not require more than 1000 nodes for 1000 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.55),
				"Average CPU utilization should be greater than 55%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.75),
				"Average memory utilization should be greater than 75%")

			// ========== PHASE 2: Interference TEST ==========
			By("Scaling down and scaling out interference scaling test")

			// Scale down both deployments
			smallDeployment.Spec.Replicas = lo.ToPtr(int32(250))
			largeDeployment := test.Deployment(largeOpts)
			env.ExpectUpdated(smallDeployment)
			env.ExpectCreated(largeDeployment)

			By("Monitoring scale out performance")
			interferenceReport, err := ReportScaleOutWithOutput(env, "Hostname Spread scale out Test", 750, 5*time.Minute, "hostname_spread_interference")
			Expect(err).ToNot(HaveOccurred(), "Scale out interference test should execute successfully")

			By("Validating scale out performance")
			Expect(interferenceReport.TestType).To(Equal("scale-out"), "Should be detected as scale out test")
			Expect(interferenceReport.TotalPods).To(Equal(750), "Should have 700 total pods after scale-in")

			// Consolidation assertions
			Expect(interferenceReport.TotalTime).To(BeNumerically("<", 10*time.Minute),
				"Scaling should complete within 10 minutes")
			Expect(interferenceReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.55),
				"Average CPU utilization should be greater than 55%")
			Expect(interferenceReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.75),
				"Average memory utilization should be greater than 75%")

		})
	})
})
