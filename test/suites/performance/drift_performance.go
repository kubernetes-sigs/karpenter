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
	Context("Drift Performance", func() {
		It("should efficiently handle drift replacement of pods with topology constraints", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: INITIAL DEPLOYMENT ==========
			By("Executing initial deployment for drift testing (600 pods)")

			initialActions := []Action{
				// Hostname spread deployment (forces wide node distribution)
				NewCreateDeploymentActionWithHostnameSpread("hostname-spread-app", 300, SmallResourceProfile),
				// Standard deployment without topology constraints
				NewCreateDeploymentAction("standard-app", 300, LargeResourceProfile),
			}

			initialReport, err := ExecuteActionsAndGenerateReport(initialActions, "Drift Test Initial Deployment", env, 20*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "Initial deployment should execute successfully")

			By("Validating initial deployment")
			Expect(initialReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(initialReport.TotalPods).To(Equal(600), "Should have 600 total pods")

			// Performance assertions for initial deployment
			Expect(initialReport.TotalTime).To(BeNumerically("<", 5*time.Minute),
				"Initial deployment should complete within 5 minutes")
			Expect(initialReport.TotalNodes).To(BeNumerically(">", 0),
				"Should provision nodes for the pods")

			By("Outputting initial deployment performance report")
			OutputPerformanceReport(initialReport, "drift_initial_deployment")

			// Allow system to stabilize before triggering drift
			By("Allowing system to stabilize before triggering drift")
			time.Sleep(30 * time.Second)

			// ========== PHASE 2: DRIFT TRIGGER ==========
			By("Triggering drift by updating NodePool template")

			driftActions := []Action{
				NewTriggerDriftAction("annotation", "NodePool template annotation drift test"),
			}

			driftReport, err := ExecuteActionsAndGenerateReportWithNodePool(driftActions, "Drift Performance Test", env, 25*time.Minute, nodePool)
			Expect(err).ToNot(HaveOccurred(), "Drift trigger should execute successfully")

			By("Validating drift execution")
			Expect(driftReport.TestType).To(Equal("drift"), "Should be detected as drift test")

			// Drift performance assertions
			Expect(driftReport.TotalTime).To(BeNumerically("<", 25*time.Minute),
				"Drift should complete within 15 minutes")

			By("Outputting drift performance report")
			OutputPerformanceReport(driftReport, "drift_execution")

			// ========== PHASE 3: POST-DRIFT VALIDATION ==========
			By("Validating post-drift cluster state")

			// Verify all pods are still healthy after drift
			allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
			env.EventuallyExpectHealthyPodCount(allPodsSelector, 600)

		})
	})
})
