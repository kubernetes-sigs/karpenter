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

	"sigs.k8s.io/karpenter/test/pkg/debug"

	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Self Anti-Affinity Deployment Interference", func() {
		It("should efficiently scale two deployments with self anti-affinity", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: SCALE-OUT TEST WITH SELF ANTI-AFFINITY ==========
			By("Creating deployments with self anti-affinity")

			// Create deployment options using templates
			smallOpts := test.CreateDeploymentOptions("small-resource-app", 500, "900m", "3100Mi",
				test.WithPodAntiAffinityHostname())
			largeOpts := test.CreateDeploymentOptions("large-resource-app", 500, "3500m", "28Gi", test.WithPodAntiAffinityHostname())

			// Create 1st deployment
			smallDeployment := test.Deployment(smallOpts)

			env.ExpectCreated(smallDeployment)

			By("Monitoring scale-out performance with self anti-affinity (500 pods)")
			scaleOutReport, err := ReportScaleOutWithOutput(env, "Self Anti-Affinity Performance Test", 500, 15*time.Minute, "self_antiaffinity_scale_out_small")
			Expect(err).ToNot(HaveOccurred(), "Scale-out should execute successfully")

			By("Validating scale-out performance with self anti-affinity")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(500), "Should have 500 total pods")

			// Performance assertions - self anti-affinity requires one pod per node
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 5*time.Minute),
				"Total scale-out time should be less than 5 minutes")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.38),
				"Average CPU utilization should be greater than 38%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.40),
				"Average memory utilization should be greater than 40%")
			Expect(scaleOutReport.KarpenterP95MemoryMB).To(BeNumerically("<", 620+MemoryOverheadMB()),
				"Karpenter controller P95 memory should be less than 620 MB during scale-out")
			Expect(scaleOutReport.KarpenterAvgCPUCores).To(BeNumerically("<", 0.90+CPUOverheadCores()),
				"Karpenter controller avg CPU should be less than 0.90 cores during scale-out")

			// ========== PHASE 2: Interference Scale Out TEST ==========
			By("Net scaling out interference test")

			// Scale down one deployment 50% and Scale up the 2nd to 500
			smallDeployment.Spec.Replicas = new(int32(250))
			largeDeployment := test.Deployment(largeOpts)
			env.ExpectUpdated(smallDeployment)
			env.ExpectCreated(largeDeployment)

			By("Monitoring scale out performance")
			interferenceReport, err := ReportScaleOutWithOutput(env, "Self Anti-Affinity scale out Test", 750, 5*time.Minute, "self_antiaffinity_interference")
			Expect(err).ToNot(HaveOccurred(), "Scale out interference test should execute successfully")

			By("Validating scale out performance")
			Expect(interferenceReport.TestType).To(Equal("scale-out"), "Should be detected as scale out test")
			Expect(interferenceReport.TotalPods).To(Equal(750), "Should have 750 total pods after scale-in")

			// Consolidation assertions
			Expect(interferenceReport.TotalTime).To(BeNumerically("<", 10*time.Minute),
				"Scaling should complete within 10 minutes")
			Expect(interferenceReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.38),
				"Average CPU utilization should be greater than 38%")
			Expect(interferenceReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.40),
				"Average memory utilization should be greater than 40%")
			Expect(interferenceReport.KarpenterP95MemoryMB).To(BeNumerically("<", 1205+MemoryOverheadMB()),
				"Karpenter controller P95 memory should be less than 1205 MB during interference")
			Expect(interferenceReport.KarpenterAvgCPUCores).To(BeNumerically("<", 1.30+CPUOverheadCores()),
				"Karpenter controller avg CPU should be less than 1.30 cores during interference")

			// ========== PHASE 3: Interference Scale In TEST ==========
			By("Executing interference consolidation test (small_deployment scales out to 400, large_deployment scales in to 200)")

			// Capture initial state before Phase 3 scaling operations
			initialNodes := interferenceReport.TotalNodes

			// Scale small_deployment from 250 to 400 (+150 pods)
			// Scale large_deployment from 500 to 200 (-300 pods)
			// Net result: 600 total pods (down from 750, net change of -150 pods)
			smallDeployment.Spec.Replicas = new(int32(400))
			largeDeployment.Spec.Replicas = new(int32(200))
			env.ExpectUpdated(smallDeployment, largeDeployment)

			By("Monitoring consolidation activity during mixed scaling operations")
			consolidationReport, err := ReportConsolidationWithOutput(env, "Interference Consolidation Test", 750, 600, initialNodes, 15*time.Minute, "self_antiaffinity_interference_consolidation")
			Expect(err).ToNot(HaveOccurred(), "Interference consolidation test should execute successfully")

			By("Validating consolidation performance during interference")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(600), "Should have 600 total pods after mixed scaling")
			Expect(consolidationReport.PodsNetChange).To(Equal(-150), "Should have net reduction of 150 pods")

			// Consolidation performance assertions
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 25*time.Minute),
				"Mixed scaling and consolidation should complete within 25 minutes")
			Expect(consolidationReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.38),
				"Average CPU utilization should remain greater than 38% after consolidation")
			Expect(consolidationReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.40),
				"Average memory utilization should remain greater than 40% after consolidation")
			Expect(consolidationReport.KarpenterP95MemoryMB).To(BeNumerically("<", 820+MemoryOverheadMB()),
				"Karpenter controller P95 memory should be less than 820 MB during consolidation")
			Expect(consolidationReport.KarpenterAvgCPUCores).To(BeNumerically("<", 0.80+CPUOverheadCores()),
				"Karpenter controller avg CPU should be less than 0.80 cores during consolidation")

		})
	})
})
