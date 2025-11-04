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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// createWideDeploymentActions creates actions for all 30 wide deployments
func createWideDeploymentActions() []Action {
	configs := getDeterministicDeploymentConfigs()
	actions := make([]Action, 30)

	for i, config := range configs {
		// Create custom resource profile for this deployment
		profile := ResourceProfile{
			CPU:    config.CPU,
			Memory: config.Memory,
		}

		// Create base action
		action := NewCreateDeploymentAction(config.Name, config.InitialPods, profile)

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

			// Add label to indicate hostname spreading
			action.Labels["has-hostname-spread"] = "true"
		}

		// Set topology constraints
		action.SetTopologySpreadConstraints(zoneConstraints)

		// Add pod anti-affinity for deployments 1, 2, 3
		if config.HasAntiAffinity {
			antiAffinity := &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"has-anti-affinity": "true",
							},
						},
						TopologyKey: corev1.LabelHostname,
					},
				},
			}
			action.SetPodAntiAffinity(antiAffinity)

			// Add label to indicate anti-affinity
			action.Labels["has-anti-affinity"] = "true"
		}

		// Add deployment index label
		action.Labels["deployment-index"] = fmt.Sprintf("%d", i+1)

		actions[i] = action
	}

	return actions
}

// createWideDeploymentScaleActions creates scale-down actions for all 30 deployments
func createWideDeploymentScaleActions() []Action {
	configs := getDeterministicDeploymentConfigs()
	actions := make([]Action, 30)

	for i, config := range configs {
		actions[i] = NewUpdateReplicasAction(config.Name, config.ScaleInPods)
	}

	return actions
}

var _ = Describe("Performance", func() {
	Context("Wide Deployments", func() {
		It("should efficiently scale 30 deployments with varied resources and topology constraints", func() {
			By("Setting up NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			// ========== PHASE 1: WIDE SCALE-OUT TEST ==========
			By("Executing wide scale-out performance test (30 deployments, 1000 pods)")

			scaleOutActions := createWideDeploymentActions()

			scaleOutReport, err := ExecuteActionsAndGenerateReport(scaleOutActions, "Wide Deployments Performance Test", env, 20*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "Wide scale-out actions should execute successfully")

			By("Validating wide scale-out performance")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(1000), "Should have 1000 total pods")

			// Performance assertions for wide deployments
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", 20*time.Minute),
				"Total scale-out time should be less than 20 minutes")
			Expect(scaleOutReport.TotalNodes).To(BeNumerically("<", 1000),
				"Should not require more than 1000 nodes for 1000 pods")
			Expect(scaleOutReport.TotalReservedCPUUtil).To(BeNumerically(">", 0.3),
				"Average CPU utilization should be greater than 30%")
			Expect(scaleOutReport.TotalReservedMemoryUtil).To(BeNumerically(">", 0.3),
				"Average memory utilization should be greater than 30%")

			By("Outputting wide scale-out performance report")
			OutputPerformanceReport(scaleOutReport, "wide_deployments_scale_out")

			// ========== PHASE 2: WIDE CONSOLIDATION TEST ==========
			By("Executing wide consolidation performance test (scaling down to 700 pods)")

			consolidationActions := createWideDeploymentScaleActions()

			consolidationReport, err := ExecuteActionsAndGenerateReport(consolidationActions, "Wide Deployments Consolidation Test", env, 25*time.Minute)
			Expect(err).ToNot(HaveOccurred(), "Wide consolidation actions should execute successfully")

			By("Validating wide consolidation performance")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(700), "Should have 700 total pods after scale-in")
			Expect(consolidationReport.PodsNetChange).To(Equal(-300), "Should have net reduction of 300 pods")
			Expect(consolidationReport.Rounds).To(BeNumerically(">", 0), "Should have consolidation rounds")

			// Wide consolidation assertions
			Expect(consolidationReport.NodesNetChange).To(BeNumerically("<", 0),
				"Node count should decrease after consolidation")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", 25*time.Minute),
				"Wide consolidation should complete within 25 minutes")

			By("Outputting wide consolidation performance report")
			OutputPerformanceReport(consolidationReport, "wide_deployments_consolidation")
		})
	})
})
