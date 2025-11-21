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
	Context("Basic Deployment", func() {
		It("should efficiently scale two deployments with different resource profiles", func() {
			// ========== PHASE 1: SCALE-OUT TEST ==========
			By("Executing scale-out performance test with 1000 pods")
			// Create deployments directly using the template
			smallDeploymentOpts := test.CreateDeploymentOptions("small-resource-app", 500, "950m", "3900Mi")
			largeDeploymentOpts := test.CreateDeploymentOptions("large-resource-app", 500, "3800m", "31Gi")

			smallDeployment := test.Deployment(smallDeploymentOpts)
			largeDeployment := test.Deployment(largeDeploymentOpts)

			env.ExpectCreated(nodePool, nodeClass, smallDeployment, largeDeployment)

			scaleOutReport, err := ReportScaleOutWithOutput(env, "Scale Out Test", 1000, 15*time.Minute, "scale_out")
			Expect(err).ToNot(HaveOccurred())
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(1000), "Should have 1000 total pods")

			// Performance assertions
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 2*time.Minute),
				"Total scale-out time should be less than 3 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 200),
				"Should not require more than 50 nodes for 1000 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.85),
				"Average CPU utilization should be greater than 75%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.75),
				"Average memory utilization should be greater than 75%")

			// ========== PHASE 2: CONSOLIDATION TEST ==========
			By("Executing consolidation performance test (scaling down to 700 pods)")
			// Phase 2: Scale-down and consolidation
			initialNodes := scaleOutReport.TotalNodes

			// Update deployments to scale down
			smallDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			largeDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			env.ExpectUpdated(smallDeployment, largeDeployment)

			consolidationReport, err := ReportConsolidationWithOutput(env, "Consolidation Test", 1000, 700, initialNodes, 20*time.Minute, "consolidation")
			Expect(err).ToNot(HaveOccurred())
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(700), "Should have 700 total pods after scale-in")
			Expect(consolidationReport.PodsNetChange).To(Equal(-300), "Should have net reduction of 300 pods")
			//Expect(consolidationReport.Rounds).To(BeNumerically(">", 0), "Should have consolidation rounds")

			// Consolidation assertions
			Expect(consolidationReport.NodesNetChange).To(BeNumerically("<", 0),
				"Node count should decrease after consolidation")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 15*time.Minute),
				"Consolidation should complete within 10 minutes")

		})
	})
})
