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
	"fmt"
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
			By("Creating initial deployments for drift testing")

			// Create deployment options using templates
			hostnameSpreadOpts := test.CreateDeploymentOptions("hostname-spread-app", 300, "950m", "3900Mi",
				test.WithHostnameSpread())
			standardOpts := test.CreateDeploymentOptions("standard-app", 300, "3800m", "31Gi")

			// Create deployments
			hostnameSpreadDeployment := test.Deployment(hostnameSpreadOpts)
			standardDeployment := test.Deployment(standardOpts)

			env.ExpectCreated(hostnameSpreadDeployment, standardDeployment)

			By("Monitoring initial deployment performance (600 pods)")
			initialReport, err := ReportScaleOutWithOutput(env, "Drift Test Initial Deployment", 600, 20*time.Minute, "drift_initial_deployment")
			Expect(err).ToNot(HaveOccurred(), "Initial deployment should execute successfully")

			By("Validating initial deployment")
			Expect(initialReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(initialReport.TotalPods).To(Equal(600), "Should have 600 total pods")

			// Performance assertions for initial deployment
			Expect(initialReport.TotalTime).To(BeNumerically("<", 5*time.Minute),
				"Initial deployment should complete within 5 minutes")
			Expect(initialReport.TotalNodes).To(BeNumerically(">", 0),
				"Should provision nodes for the pods")

			// Allow system to stabilize before triggering drift
			By("Allowing system to stabilize before triggering drift")
			time.Sleep(30 * time.Second)

			// ========== PHASE 2: DRIFT TRIGGER AND MONITORING ==========
			By("Triggering drift by updating NodePool template")

			// Trigger drift by updating the NodePool template annotation
			if nodePool.Spec.Template.Annotations == nil {
				nodePool.Spec.Template.Annotations = make(map[string]string)
			}
			nodePool.Spec.Template.Annotations["test-drift-trigger"] = fmt.Sprintf("drift-%d", time.Now().Unix())
			env.ExpectUpdated(nodePool)

			By("Monitoring drift performance")
			driftReport, err := ReportDriftWithOutput(env, "Drift Performance Test", 600, 25*time.Minute, "drift_execution")
			Expect(err).ToNot(HaveOccurred(), "Drift should execute successfully")

			By("Validating drift execution")
			Expect(driftReport.TestType).To(Equal("drift"), "Should be detected as drift test")
			Expect(driftReport.TotalPods).To(Equal(600), "Should maintain 600 pods during drift")
			Expect(driftReport.PodsNetChange).To(Equal(0), "Pods should not change during drift")

			// Drift performance assertions
			Expect(driftReport.TotalTime).To(BeNumerically("<", 25*time.Minute),
				"Drift should complete within 25 minutes")
			Expect(driftReport.Rounds).To(BeNumerically(">", 0),
				"Should have at least one drift replacement round")

			// ========== PHASE 3: POST-DRIFT VALIDATION ==========
			By("Validating post-drift cluster state")

			// Verify all pods are still healthy after drift
			allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
			env.EventuallyExpectHealthyPodCount(allPodsSelector, 600)

		})
	})
})
