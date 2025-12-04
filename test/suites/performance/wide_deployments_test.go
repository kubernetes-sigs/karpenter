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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/test"
)

// DeploymentConfig holds the deterministic configuration for each deployment
type DeploymentConfig struct {
	Name                      string
	CPU                       string
	Memory                    string
	InitialPods               int32
	ScaleInPods               int32
	HasAntiAffinity           bool
	HasHostnameTopologySpread bool
}

// getDeterministicDeploymentConfigs returns fixed, deterministic configurations for all 30 deployments
func getDeterministicDeploymentConfigs() []DeploymentConfig {
	// Deterministic CPU values (0.25 - 8.0 vCPU range)
	cpuValues := []string{
		"250m", "500m", "750m", "1000m", "1500m", "2000m", "2500m", "3000m", "3500m", "4000m", // 1-10
		"2500m", "1000m", "1500m", "3000m", "1800m", "2000m", "3500m", "4000m", "250m", "500m", // 11-20
		"750m", "1000m", "1500m", "2000m", "2500m", "3000m", "3500m", "4000m", "3500m", "3000m", // 21-30
	}

	// Deterministic memory values (300MB - 64GB range)
	memoryValues := []string{
		"300Mi", "500Mi", "750Mi", "1Gi", "2Gi", "4Gi", "8Gi", "4Gi", "16Gi", "10Gi", // 1-10
		"64Gi", "30Gi", "32Gi", "32Gi", "30Gi", "20Gi", "30Gi", "300Mi", "500Mi", "750Mi", // 11-20
		"1Gi", "2Gi", "4Gi", "8Gi", "16Gi", "32Gi", "28Gi", "64Gi", "13Gi", "24Gi", // 21-30
	}

	// Deterministic pod counts - sum to exactly 1000 initially, 700 after scale-in
	initialPodCounts := []int32{
		35, 35, 35, 35, 35, 35, 35, 35, 35, 35, // 1-10: 350 total
		35, 35, 35, 35, 35, 35, 35, 35, 35, 35, // 11-20: 350 total
		30, 30, 30, 30, 30, 30, 30, 30, 30, 30, // 21-30: 300 total
	} // Total: 1000 pods

	scaleInPodCounts := []int32{
		25, 25, 25, 25, 25, 25, 25, 25, 25, 25, // 1-10: 250 total
		25, 25, 25, 25, 25, 25, 25, 25, 25, 25, // 11-20: 250 total
		20, 20, 20, 20, 20, 20, 20, 20, 20, 20, // 21-30: 200 total
	} // Total: 700 pods

	configs := make([]DeploymentConfig, 30)
	for i := 0; i < 30; i++ {
		configs[i] = DeploymentConfig{
			Name:                      fmt.Sprintf("wide-deployment-%d", i+1),
			CPU:                       cpuValues[i],
			Memory:                    memoryValues[i],
			InitialPods:               initialPodCounts[i],
			ScaleInPods:               scaleInPodCounts[i],
			HasAntiAffinity:           i < 3,           // Deployments 1, 2, 3
			HasHostnameTopologySpread: i >= 3 && i < 6, // Deployments 4, 5, 6
		}
	}

	return configs
}

// createWideDeployments creates all 30 wide deployments using the new template approach
func createWideDeployments() []*appsv1.Deployment {
	configs := getDeterministicDeploymentConfigs()
	deployments := make([]*appsv1.Deployment, 30)

	for i, config := range configs {
		// Build modifiers based on configuration
		var modifiers []test.DeploymentOptionModifier

		// Add zone topology spreading (all deployments get this)
		zoneConstraints := []corev1.TopologySpreadConstraint{
			{
				MaxSkew:           1,
				TopologyKey:       "topology.kubernetes.io/zone",
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": config.Name,
					},
				},
			},
		}

		// Add hostname topology spreading for deployments 4, 5, 6
		if config.HasHostnameTopologySpread {
			hostnameConstraint := corev1.TopologySpreadConstraint{
				MaxSkew:           1,
				TopologyKey:       corev1.LabelHostname,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": config.Name,
					},
				},
			}
			zoneConstraints = append(zoneConstraints, hostnameConstraint)
			modifiers = append(modifiers, test.WithLabels(map[string]string{"has-hostname-spread": "true"}))
		}

		modifiers = append(modifiers, test.WithTopologySpreadConstraints(zoneConstraints))

		// Add pod anti-affinity for deployments 1, 2, 3
		if config.HasAntiAffinity {
			modifiers = append(modifiers,
				test.WithPodAntiAffinity(corev1.LabelHostname),
				test.WithLabels(map[string]string{"has-anti-affinity": "true"}))
		}

		// Add deployment index label
		modifiers = append(modifiers, test.WithLabels(map[string]string{
			"deployment-index": fmt.Sprintf("%d", i+1),
		}))

		// Create deployment options using templates
		opts := test.CreateDeploymentOptions(config.Name, config.InitialPods, config.CPU, config.Memory, modifiers...)
		deployments[i] = test.Deployment(opts)
	}

	return deployments
}

var _ = Describe("Performance", func() {
	Context("Wide Deployments", func() {
		It("should efficiently scale 30 deployments with varied resources and topology constraints", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: WIDE SCALE-OUT TEST ==========
			By("Creating 30 wide deployments with varied configurations")

			// Create all 30 deployments using the new template approach
			deployments := createWideDeployments()

			// Create all deployments in the cluster
			for _, dep := range deployments {
				env.ExpectCreated(dep)
			}

			By("Monitoring wide scale-out performance (30 deployments, 1000 pods)")
			scaleOutReport, err := ReportScaleOutWithOutput(env, "Wide Deployments Performance Test", 1000, 20*time.Minute, "wide_deployments_scale_out")
			Expect(err).ToNot(HaveOccurred(), "Wide scale-out should execute successfully")

			By("Validating wide scale-out performance")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(1000), "Should have 1000 total pods")

			// Performance assertions for wide deployments
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 3*time.Minute),
				"Total scale-out time should be less than 3 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 300),
				"Should not require more than 1000 nodes for 1000 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.68),
				"Average CPU utilization should be greater than 68%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.55),
				"Average memory utilization should be greater than 55%")

			// ========== PHASE 2: WIDE CONSOLIDATION TEST ==========
			By("Scaling down all 30 deployments to trigger consolidation")
			initialNodes := scaleOutReport.TotalNodes
			configs := getDeterministicDeploymentConfigs()

			// Scale down all deployments to their scale-in pod counts
			for i, deployment := range deployments {
				deployment.Spec.Replicas = &configs[i].ScaleInPods
				env.ExpectUpdated(deployment)
			}

			By("Monitoring wide consolidation performance")
			consolidationReport, err := ReportConsolidationWithOutput(env, "Wide Deployments Consolidation Test", 1000, 700, initialNodes, 25*time.Minute, "wide_deployments_consolidation")
			Expect(err).ToNot(HaveOccurred(), "Wide consolidation should execute successfully")

			By("Validating wide consolidation performance")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(700), "Should have 700 total pods after scale-in")
			Expect(consolidationReport.PodsNetChange).To(Equal(-300), "Should have net reduction of 300 pods")

			// Wide consolidation assertions
			Expect(consolidationReport.NodesNetChange).To(BeNumerically("<", 0),
				"Node count should decrease after consolidation")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 10*time.Minute),
				"Wide consolidation should complete within 10 minutes")
			Expect(consolidationReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.65),
				"Average CPU utilization should be greater than 65%")
			Expect(consolidationReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.65),
				"Average memory utilization should be greater than 65%")

		})
	})
})
