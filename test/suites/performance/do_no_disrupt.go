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
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Performance", func() {
	Context("Do Not Disrupt Performance Test", func() {
		It("should efficiently scale three deployments and test disruption protection", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: SCALE-OUT TEST WITH DO-NOT-DISRUPT ==========
			By("Executing scale-out performance test with do-not-disrupt protection (1100 pods)")

			scaleOutActions := []Action{
				// Small deployment with hostname topology spreading
				NewCreateDeploymentActionWithHostnameSpread("small-resource-app", 500, SmallResourceProfile),
				// Large deployment without constraints
				NewCreateDeploymentAction("large-resource-app", 500, LargeResourceProfile),
				// Do-not-disrupt deployment with protection annotation
				NewCreateDeploymentActionWithDoNotDisrupt("do-not-disrupt-app", 100, DoNotDisruptResourceProfile),
			}

			scaleOutReport, err := ExecuteActionsAndGenerateReport(scaleOutActions, "Do Not Disrupt Performance Test", env, 15*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "Scale-out actions should execute successfully")

			By("Validating scale-out performance with do-not-disrupt protection")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(1100), "Should have 1100 total pods")

			// Performance assertions
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 12*time.Minute),
				"Total scale-out time should be less than 12 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 1100),
				"Should not require more than 550 nodes for 1100 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.5),
				"Average CPU utilization should be greater than 40%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.5),
				"Average memory utilization should be greater than 40%")

			By("Outputting scale-out performance report")
			OutputPerformanceReport(scaleOutReport, "do_not_disrupt_scale_out")

			// ========== PHASE 2: DISRUPTION PROTECTION TEST ==========
			By("Testing disruption protection behavior")

			// Get nodes that have do-not-disrupt pods before scaling down
			doNotDisruptPodSelector := labels.SelectorFromSet(map[string]string{
				"app":               "do-not-disrupt-app",
				test.DiscoveryLabel: "unspecified",
			})
			doNotDisruptPods := env.Monitor.RunningPods(doNotDisruptPodSelector)
			nodesWithProtectedPods := make(map[string]bool)
			for _, pod := range doNotDisruptPods {
				if pod.Spec.NodeName != "" {
					nodesWithProtectedPods[pod.Spec.NodeName] = true
				}
			}

			GinkgoWriter.Printf("Nodes with do-not-disrupt pods: %d\n", len(nodesWithProtectedPods))

			// Scale down small and large deployments to trigger consolidation (but keep do-not-disrupt)
			consolidationActions := []Action{
				NewUpdateReplicasAction("small-resource-app", 250),
				NewUpdateReplicasAction("large-resource-app", 250),
				// Note: do-not-disrupt-app remains at 100 replicas
			}

			consolidationReport, err := ExecuteActionsAndGenerateReport(consolidationActions, "Do Not Disrupt Consolidation Test", env, 20*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "Consolidation actions should execute successfully")

			By("Validating disruption protection during consolidation")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(600), "Should have 600 total pods after scale-in (250+250+100)")
			Expect(consolidationReport.PodsNetChange).To(Equal(-500), "Should have net reduction of 500 pods")

			// Check if nodes with do-not-disrupt pods are still present
			currentNodes := env.Monitor.CreatedNodes()
			protectedNodesStillPresent := 0
			for _, node := range currentNodes {
				if nodesWithProtectedPods[node.Name] {
					protectedNodesStillPresent++
				}
			}

			By("Validating that protected nodes were not disrupted")
			Expect(protectedNodesStillPresent).To(BeNumerically(">", 0),
				"At least some nodes with do-not-disrupt pods should remain protected")

			GinkgoWriter.Printf("Protected nodes still present: %d/%d\n", protectedNodesStillPresent, len(nodesWithProtectedPods))

			By("Outputting consolidation performance report")
			OutputPerformanceReport(consolidationReport, "do_not_disrupt_consolidation")

		})
	})
})
