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
	Context("Host Name Spreading Deployment XL", func() {
		It("should efficiently scale two deployments with host name topology spreading at XL scale", func() {
			By("Setting up NodePool and NodeClass for the XL test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: XL SCALE-OUT TEST WITH HOSTNAME SPREADING ==========
			By("Executing XL scale-out performance test with hostname spreading (2000 pods)")

			scaleOutActions := []Action{
				// Small deployment with hostname topology spreading - XL scale (1000 pods)
				NewCreateDeploymentActionWithHostnameSpread("small-resource-app", 1000, SmallResourceProfile),
				// Large deployment without topology constraints - XL scale (1000 pods)
				NewCreateDeploymentAction("large-resource-app", 1000, LargeResourceProfile),
			}

			scaleOutReport, err := ExecuteActionsAndGenerateReport(scaleOutActions, "XL Host Name Spreading Performance Test", env, 20*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "XL scale-out actions should execute successfully")

			By("Validating XL scale-out performance with hostname spreading")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(2000), "Should have 2000 total pods")

			// XL Performance assertions - hostname spreading at scale may require many more nodes
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 20*time.Minute),
				"Total XL scale-out time should be less than 20 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 2000),
				"Should not require more than 2000 nodes for 2000 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.4),
				"Average CPU utilization should be greater than 40%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.4),
				"Average memory utilization should be greater than 40%")

			By("Outputting XL scale-out performance report")
			OutputPerformanceReport(scaleOutReport, "hostname_spread_xl_scale_out")

			// ========== PHASE 2: XL CONSOLIDATION TEST ==========
			By("Executing XL consolidation performance test (scaling down to 1400 pods)")

			consolidationActions := []Action{
				NewUpdateReplicasAction("small-resource-app", 700),
				NewUpdateReplicasAction("large-resource-app", 700),
			}

			consolidationReport, err := ExecuteActionsAndGenerateReport(consolidationActions, "XL Hostname Spread Consolidation Test", env, 25*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "XL consolidation actions should execute successfully")

			By("Validating XL consolidation performance")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(1400), "Should have 1400 total pods after scale-in")
			Expect(consolidationReport.PodsNetChange).To(Equal(-600), "Should have net reduction of 600 pods")
			//Expect(consolidationReport.Rounds).To(BeNumerically(">", 0), "Should have consolidation rounds")

			// XL Consolidation assertions
			Expect(consolidationReport.NodesNetChange).To(BeNumerically("<", 0),
				"Node count should decrease after consolidation")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 25*time.Minute),
				"XL consolidation should complete within 25 minutes")

			By("Outputting XL consolidation performance report")
			OutputPerformanceReport(consolidationReport, "hostname_spread_xl_consolidation")

		})
	})
})
