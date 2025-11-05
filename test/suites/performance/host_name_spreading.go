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
)

var _ = Describe("Performance", func() {
	Context("Host Name Spreading Deployment Reg", func() {
		It("should efficiently scale two deployments with host name topology spreading", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: SCALE-OUT TEST WITH HOSTNAME SPREADING ==========
			By("Executing scale-out performance test with hostname spreading (1000 pods)")

			scaleOutActions := []Action{
				// Small deployment with hostname topology spreading
				NewCreateDeploymentActionWithHostnameSpread("small-resource-app", 500, SmallResourceProfile),
				// Large deployment without topology constraints
				NewCreateDeploymentAction("large-resource-app", 500, LargeResourceProfile),
			}

			scaleOutReport, err := ExecuteActionsAndGenerateReport(scaleOutActions, "Host Name Spreading Performance Test", env, 15*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "Scale-out actions should execute successfully")

			By("Validating scale-out performance with hostname spreading")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(1000), "Should have 1000 total pods")

			// Performance assertions - hostname spreading may require more nodes
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 10*time.Minute),
				"Total scale-out time should be less than 10 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 1000),
				"Should not require more than 1000 nodes for 1000 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.4),
				"Average CPU utilization should be greater than 40%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.4),
				"Average memory utilization should be greater than 40%")

			By("Outputting scale-out performance report")
			OutputPerformanceReport(scaleOutReport, "hostname_spread_scale_out")

			// ========== PHASE 2: CONSOLIDATION TEST ==========
			By("Executing consolidation performance test (scaling down to 700 pods)")

			consolidationActions := []Action{
				NewUpdateReplicasAction("small-resource-app", 350),
				NewUpdateReplicasAction("large-resource-app", 350),
			}

			consolidationReport, err := ExecuteActionsAndGenerateReport(consolidationActions, "Hostname Spread Consolidation Test", env, 20*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "Consolidation actions should execute successfully")

			By("Validating consolidation performance")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(700), "Should have 700 total pods after scale-in")
			Expect(consolidationReport.PodsNetChange).To(Equal(-300), "Should have net reduction of 300 pods")
			//Expect(consolidationReport.Rounds).To(BeNumerically(">", 0), "Should have consolidation rounds")

			// Consolidation assertions
			Expect(consolidationReport.NodesNetChange).To(BeNumerically("<", 0),
				"Node count should decrease after consolidation")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 20*time.Minute),
				"Consolidation should complete within 20 minutes")

			By("Outputting consolidation performance report")
			OutputPerformanceReport(consolidationReport, "hostname_spread_consolidation")

		})
	})
})
