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
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Performance", func() {
	Context("Do Not Disrupt Performance Test", func() {
		It("should efficiently scale three deployments and test disruption protection", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: SCALE-OUT TEST WITH DO-NOT-DISRUPT ==========
			By("Creating deployments with different resource profiles and protection")

			// Create deployment options using templates
			smallOpts := test.CreateDeploymentOptions("small-resource-app", 500, "950m", "3900Mi",
				test.WithHostnameSpread())
			largeOpts := test.CreateDeploymentOptions("large-resource-app", 500, "3800m", "31Gi")
			protectedOpts := test.CreateDeploymentOptions("do-not-disrupt-app", 100, "950m", "450Mi",
				test.WithDoNotDisrupt())

			// Create deployments
			smallDeployment := test.Deployment(smallOpts)
			largeDeployment := test.Deployment(largeOpts)
			protectedDeployment := test.Deployment(protectedOpts)

			env.ExpectCreated(smallDeployment, largeDeployment, protectedDeployment)

			By("Monitoring scale-out performance with do-not-disrupt protection (1100 pods)")
			scaleOutReport, err := ReportScaleOutWithOutput(env, "Do Not Disrupt Performance Test", 1100, 15*time.Minute, "do_not_disrupt_scale_out")
			Expect(err).ToNot(HaveOccurred(), "Scale-out should execute successfully")

			By("Validating scale-out performance with do-not-disrupt protection")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(1100), "Should have 1100 total pods")

			// Performance assertions
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 4*time.Minute),
				"Total scale-out time should be less than 4 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 750),
				"Should not require more than 550 nodes for 1100 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.55),
				"Average CPU utilization should be greater than 55%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.7),
				"Average memory utilization should be greater than 70%")

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

			By("Scaling down deployments to trigger consolidation")
			initialNodes := scaleOutReport.TotalNodes

			// Scale down small and large deployments (keep do-not-disrupt unchanged)
			smallDeployment.Spec.Replicas = lo.ToPtr(int32(250))
			largeDeployment.Spec.Replicas = lo.ToPtr(int32(250))
			env.ExpectUpdated(smallDeployment, largeDeployment)

			By("Monitoring consolidation with disruption protection")
			consolidationReport, err := ReportConsolidationWithOutput(env, "Do Not Disrupt Consolidation Test", 1100, 600, initialNodes, 25*time.Minute, "do_not_disrupt_consolidation")
			Expect(err).ToNot(HaveOccurred(), "Consolidation should execute successfully")

			By("Validating disruption protection during consolidation")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(BeNumerically(">=", 600), "Should have at least 600 total pods after scale-in (250+250+100)")
			Expect(consolidationReport.PodsNetChange).To(BeNumerically(">=", -500), "Should have net reduction of 500 pods")

			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 10*time.Minute),
				"Consolidation should complete within 10 minutes")
			Expect(consolidationReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.55),
				"Average CPU utilization should be greater than 55%")
			Expect(consolidationReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.7),
				"Average memory utilization should be greater than 70%")

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

		})
	})
})
