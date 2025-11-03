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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
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

	// Deterministic memory values (300MB - 160GB range)
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

var _ = Describe("Performance", func() {
	Context("Wide Deployments", func() {
		It("should efficiently scale 30 deployments with varied resources and topology constraints", func() {
			By("Setting up performance test with 1000 pods across 30 varied deployments")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("CREATING 30 WIDE DEPLOYMENTS" + "\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Record test start time
			testStartTime := time.Now()
			env.TimeIntervalCollector.Start("test_start")

			// Get deterministic deployment configurations
			deploymentConfigs := getDeterministicDeploymentConfigs()

			// Declare variables that will be used in DeferCleanup
			var deployments []*appsv1.Deployment
			var allPodsSelector labels.Selector

			// Set up cleanup that runs regardless of test outcome
			DeferCleanup(func() {
				if len(deployments) > 0 {
					By("Emergency cleanup - ensuring test resources are removed")
					opts := common.DefaultCleanupOptions()
					opts.Deployments = deployments
					opts.PodSelector = allPodsSelector
					_ = env.ForceCleanupTestResources(opts)
				}
			})

			By("Creating NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			By("Creating 30 deployments with deterministic configurations")
			env.TimeIntervalCollector.Start("deployments_created")

			// Create all 30 deployments
			deployments = make([]*appsv1.Deployment, 30)
			for i, config := range deploymentConfigs {
				GinkgoWriter.Printf("Creating %s: %s CPU, %s memory, %d pods\n",
					config.Name, config.CPU, config.Memory, config.InitialPods)

				// Define resource requirements
				resources := corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(config.CPU),
						corev1.ResourceMemory: resource.MustParse(config.Memory),
					},
				}

				// Base labels for all deployments
				labels := map[string]string{
					"app":               config.Name,
					"deployment-index":  fmt.Sprintf("%d", i+1),
					test.DiscoveryLabel: "unspecified",
				}

				// Build topology spread constraints - all deployments get zone spreading
				var topologyConstraints []corev1.TopologySpreadConstraint

				// Zone topology spread (all deployments)
				zoneConstraint := corev1.TopologySpreadConstraint{
					MaxSkew:           1,
					TopologyKey:       "topology.kubernetes.io/zone",
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": config.Name,
						},
					},
				}
				topologyConstraints = append(topologyConstraints, zoneConstraint)

				// Hostname topology spread (deployments 4, 5, 6)
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
					topologyConstraints = append(topologyConstraints, hostnameConstraint)
					labels["has-hostname-spread"] = "true"
				}

				// Build pod anti-affinity requirements (deployments 1, 2, 3)
				var podAntiRequirements []corev1.PodAffinityTerm
				if config.HasAntiAffinity {
					podAntiRequirements = []corev1.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"has-anti-affinity": "true",
								},
							},
							TopologyKey: corev1.LabelHostname,
						},
					}
					labels["has-anti-affinity"] = "true"
				}

				// Create the deployment
				deployment := test.Deployment(test.DeploymentOptions{
					Replicas: config.InitialPods,
					PodOptions: test.PodOptions{
						ObjectMeta: metav1.ObjectMeta{
							Labels: labels,
						},
						ResourceRequirements:      resources,
						TopologySpreadConstraints: topologyConstraints,
						PodAntiRequirements:       podAntiRequirements,
					},
				})

				// Override the deployment name to use our deterministic naming
				deployment.Name = config.Name
				deployment.Spec.Selector.MatchLabels["app"] = config.Name
				deployment.Spec.Template.Labels["app"] = config.Name

				deployments[i] = deployment
			}

			// Create all deployments
			deploymentObjects := make([]client.Object, len(deployments))
			for i, deployment := range deployments {
				deploymentObjects[i] = deployment
			}
			env.ExpectCreated(deploymentObjects...)
			env.TimeIntervalCollector.End("deployments_created")

			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("30 DEPLOYMENTS CREATED - WAITING FOR PROVISIONING" + "\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Create selector for monitoring all pods
			allPodsSelector = labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})

			By("Waiting for all 1000 pods to be scheduled and ready")
			env.TimeIntervalCollector.Start("waiting_for_pods")

			// Add debugging information before the timeout
			By("Adding debug information for pod/node status")
			GinkgoWriter.Printf("\n🔍 DEBUGGING POD/NODE STATUS BEFORE WAITING:\n")

			// Current pod status breakdown
			runningPods := env.Monitor.RunningPods(allPodsSelector)
			pendingPods := env.Monitor.PendingPods(allPodsSelector)
			totalPods := len(runningPods) + len(pendingPods)

			GinkgoWriter.Printf("  • Running pods: %d\n", len(runningPods))
			GinkgoWriter.Printf("  • Pending pods: %d\n", len(pendingPods))
			GinkgoWriter.Printf("  • Total pods found: %d\n", totalPods)
			GinkgoWriter.Printf("  • Expected pods: 1000\n")

			// Current node status
			debugNodeCount := env.Monitor.CreatedNodeCount()
			allNodes := env.Monitor.CreatedNodes()
			readyNodes := 0
			schedulableNodes := 0

			for _, node := range allNodes {
				// Check if node is ready
				for _, condition := range node.Status.Conditions {
					if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
						readyNodes++
						break
					}
				}
				// Check if node is schedulable
				if !node.Spec.Unschedulable {
					schedulableNodes++
				}
			}

			GinkgoWriter.Printf("  • Total nodes: %d\n", debugNodeCount)
			GinkgoWriter.Printf("  • Ready nodes: %d\n", readyNodes)
			GinkgoWriter.Printf("  • Schedulable nodes: %d\n", schedulableNodes)

			// Resource utilization
			if debugNodeCount > 0 {
				avgCPU := env.Monitor.AvgUtilization(corev1.ResourceCPU)
				avgMem := env.Monitor.AvgUtilization(corev1.ResourceMemory)
				GinkgoWriter.Printf("  • Average CPU utilization: %.2f%%\n", avgCPU*100)
				GinkgoWriter.Printf("  • Average Memory utilization: %.2f%%\n", avgMem*100)
			}

			// Wait for all pods to become healthy with a 20-minute timeout
			env.EventuallyExpectHealthyPodCountWithTimeout(20*time.Minute, allPodsSelector, 1000)

			env.TimeIntervalCollector.End("waiting_for_pods")
			env.TimeIntervalCollector.End("test_start")
			totalTime := time.Since(testStartTime)

			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("SCALE OUT COMPLETED!" + "\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Allow logs to catch up before collecting metrics
			By("Waiting for system logs and metrics to stabilize")
			GinkgoWriter.Printf("⏳ Allowing 30 seconds for logs to catch up...\n")
			time.Sleep(5 * time.Second)

			By("Collecting performance metrics")

			// Get node and resource utilization metrics
			nodeCount := env.Monitor.CreatedNodeCount()
			avgCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
			avgMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)
			minCPUUtil := env.Monitor.MinUtilization(corev1.ResourceCPU)
			minMemUtil := env.Monitor.MinUtilization(corev1.ResourceMemory)

			// Calculate derived metrics
			resourceEfficiencyScore := (avgCPUUtil + avgMemUtil) * 50 // Scale to 0-100%
			podsPerNode := float64(1000) / float64(nodeCount)

			By("Generating performance report")

			// Determine test status and collect warnings
			var warnings []string
			testPassed := true

			if totalTime >= 15*time.Minute {
				warnings = append(warnings, "Total scale-out time exceeded 15 minutes")
				testPassed = false
			}
			if avgCPUUtil < 0.4 {
				warnings = append(warnings, fmt.Sprintf("Average CPU utilization below 40%% (%.1f%%)", avgCPUUtil*100))
				testPassed = false
			}
			if avgMemUtil < 0.4 {
				warnings = append(warnings, fmt.Sprintf("Average memory utilization below 40%% (%.1f%%)", avgMemUtil*100))
				testPassed = false
			}
			if nodeCount > 1000 {
				warnings = append(warnings, fmt.Sprintf("Too many nodes provisioned (>1000): %d", nodeCount))
				testPassed = false
			}

			// Create structured performance report
			report := PerformanceReport{
				TestName:                "Wide Deployments Performance Test",
				TotalPods:               1000,
				SmallPods:               0, // Will be calculated based on resource ranges
				LargePods:               0, // Will be calculated based on resource ranges
				TotalTime:               totalTime,
				NodesProvisioned:        nodeCount,
				TotalReservedCPUUtil:    avgCPUUtil,
				TotalReservedMemoryUtil: avgMemUtil,
				ResourceEfficiencyScore: resourceEfficiencyScore,
				PodsPerNode:             podsPerNode,
				ScaleInEnabled:          false, // Will be updated if scale-in is performed
				Timestamp:               time.Now(),
				TestPassed:              testPassed,
				Warnings:                warnings,
			}

			// Calculate small/large pod distribution based on resource requirements
			smallPods, largePods := 0, 0
			for _, config := range deploymentConfigs {
				cpuVal := resource.MustParse(config.CPU)
				if cpuVal.MilliValue() <= 2000 { // <= 2 vCPU considered small
					smallPods += int(config.InitialPods)
				} else {
					largePods += int(config.InitialPods)
				}
			}
			report.SmallPods = smallPods
			report.LargePods = largePods

			// Output detailed performance report to console
			By("=== PERFORMANCE TEST REPORT ===")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🚀 WIDE DEPLOYMENTS PERFORMANCE REPORT\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			// Test Configuration
			GinkgoWriter.Printf("\n📋 TEST CONFIGURATION:\n")
			GinkgoWriter.Printf("  • Total Deployments: 30\n")
			GinkgoWriter.Printf("  • Total Pods: %d\n", report.TotalPods)
			GinkgoWriter.Printf("  • Small Pods (≤2 vCPU): %d\n", report.SmallPods)
			GinkgoWriter.Printf("  • Large Pods (>2 vCPU): %d\n", report.LargePods)
			GinkgoWriter.Printf("  • Anti-affinity deployments: 3 (deployments 1-3)\n")
			GinkgoWriter.Printf("  • Hostname topology spread: 3 (deployments 4-6)\n")
			GinkgoWriter.Printf("  • Zone topology spread: 30 (all deployments)\n")
			GinkgoWriter.Printf("  • CPU range: 0.25 - 8.0 vCPU\n")
			GinkgoWriter.Printf("  • Memory range: 300MB - 160GB\n")
			GinkgoWriter.Printf("  • Test Duration: %v\n", report.TotalTime)

			// Timing Metrics
			GinkgoWriter.Printf("\n⏱️  TIMING METRICS:\n")
			GinkgoWriter.Printf("  • Total Scale-out Time: %v\n", report.TotalTime)

			// Resource Utilization
			GinkgoWriter.Printf("\n💻 RESOURCE UTILIZATION:\n")
			GinkgoWriter.Printf("  • Total Reserved CPU Utilization: %.2f%%\n", report.TotalReservedCPUUtil*100)
			GinkgoWriter.Printf("  • Total Reserved Memory Utilization: %.2f%%\n", report.TotalReservedMemoryUtil*100)
			GinkgoWriter.Printf("  • Minimum CPU Utilization: %.2f%%\n", minCPUUtil*100)
			GinkgoWriter.Printf("  • Minimum Memory Utilization: %.2f%%\n", minMemUtil*100)

			// Cost Analysis
			GinkgoWriter.Printf("\n💰 COST ANALYSIS:\n")
			GinkgoWriter.Printf("  • Total Nodes Provisioned: %d\n", report.NodesProvisioned)
			GinkgoWriter.Printf("  • Pods per Node (avg): %.1f\n", report.PodsPerNode)
			GinkgoWriter.Printf("  • Resource Efficiency Score: %.1f%%\n", report.ResourceEfficiencyScore)

			// Performance Summary Table
			GinkgoWriter.Printf("\n📊 PERFORMANCE SUMMARY:\n")
			GinkgoWriter.Printf("┌─────────────────────────────┬──────────────┬──────────┐\n")
			GinkgoWriter.Printf("│ Metric                      │ Value        │ Status   │\n")
			GinkgoWriter.Printf("├─────────────────────────────┼──────────────┼──────────┤\n")
			GinkgoWriter.Printf("│ Total Scale-out Time        │ %-12v │ %s │\n",
				report.TotalTime, getStatusIcon(report.TotalTime < 15*time.Minute))
			GinkgoWriter.Printf("│ Nodes Provisioned           │ %-12d │ %s │\n",
				report.NodesProvisioned, getStatusIcon(report.NodesProvisioned < 1000))
			GinkgoWriter.Printf("│ Total Reserved CPU Util     │ %-11.1f%% │ %s │\n",
				report.TotalReservedCPUUtil*100, getStatusIcon(report.TotalReservedCPUUtil > 0.4))
			GinkgoWriter.Printf("│ Total Reserved Memory Util  │ %-11.1f%% │ %s │\n",
				report.TotalReservedMemoryUtil*100, getStatusIcon(report.TotalReservedMemoryUtil > 0.4))
			GinkgoWriter.Printf("│ Resource Efficiency Score   │ %-11.1f%% │ %s │\n",
				report.ResourceEfficiencyScore, getStatusIcon(report.ResourceEfficiencyScore > 40))
			GinkgoWriter.Printf("└─────────────────────────────┴──────────────┴──────────┘\n")

			// Test Verdict
			if report.TestPassed {
				GinkgoWriter.Printf("\n✅ PERFORMANCE TEST: PASSED\n")
				GinkgoWriter.Printf("   All performance targets met successfully!\n")
			} else {
				GinkgoWriter.Printf("\n⚠️  PERFORMANCE TEST: ATTENTION NEEDED\n")
				for _, warning := range report.Warnings {
					GinkgoWriter.Printf("   • %s\n", warning)
				}
			}

			By("Validating performance assertions")

			// Performance Assertions (adjusted for 30 deployments)
			Expect(totalTime).To(BeNumerically("<", 15*time.Minute),
				"Total scale-out time should be less than 15 minutes")

			Expect(nodeCount).To(BeNumerically("<", 1000),
				"Should not require more than 1000 nodes for 1000 pods")

			Expect(avgCPUUtil).To(BeNumerically(">", 0.3),
				"Average CPU utilization should be greater than 30%")

			Expect(avgMemUtil).To(BeNumerically(">", 0.3),
				"Average memory utilization should be greater than 30%")

			// Verify all pods are actually running for each deployment
			for i, deployment := range deployments {
				selector := labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels)
				env.EventuallyExpectHealthyPodCount(selector, int(deploymentConfigs[i].InitialPods))
			}

			// ========== SCALE-IN PERFORMANCE TEST ==========
			By("Starting scale-in performance test")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🔽 SCALE-IN PERFORMANCE TEST: Scaling down to 70%% (700 pods)\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			// Record pre-scale-in state
			preScaleInNodes := env.Monitor.CreatedNodeCount()

			// Scale down all deployments according to deterministic scale-in configuration
			By("Scaling all 30 deployments down to 700 total pods")
			for i, deployment := range deployments {
				deployment.Spec.Replicas = lo.ToPtr(deploymentConfigs[i].ScaleInPods)
				GinkgoWriter.Printf("Scaling %s from %d to %d pods\n",
					deploymentConfigs[i].Name, deploymentConfigs[i].InitialPods, deploymentConfigs[i].ScaleInPods)
			}

			deploymentObjectsForUpdate := make([]client.Object, len(deployments))
			for i, deployment := range deployments {
				deploymentObjectsForUpdate[i] = deployment
			}
			env.ExpectUpdated(deploymentObjectsForUpdate...)
			GinkgoWriter.Printf("   • All 30 deployments scaled down to total 700 pods\n")

			// Wait for pods to be terminated
			By("Waiting for pods to scale down")
			Eventually(func(g Gomega) {
				pods := env.Monitor.RunningPods(allPodsSelector)
				g.Expect(pods).To(HaveLen(700), "Should have exactly 700 pods after scale-in")
			}).WithTimeout(5 * time.Minute).Should(Succeed())
			GinkgoWriter.Printf("   • Pod count reduced to 700\n")

			// Monitor consolidation rounds
			By("Monitoring node consolidation")
			var consolidationRounds []ConsolidationRound
			roundNumber := 1
			lastDrainingTime := time.Now()
			consolidationStartTime := time.Now()

			// Track consolidation with 15-minute timeout
			consolidationComplete := false
			consolidationTimeout := 15 * time.Minute

			for time.Since(consolidationStartTime) < consolidationTimeout && !consolidationComplete {
				currentNodes := env.Monitor.CreatedNodeCount()
				GinkgoWriter.Printf("Checking for consolidation activity\n")
				GinkgoWriter.Printf("  • Pre-scale-in nodes: %d\n", preScaleInNodes)
				GinkgoWriter.Printf("  • Current nodes: %d\n", currentNodes)

				// Check if nodes are draining/terminating
				var drainingNodes []corev1.Node
				allNodes := env.Monitor.CreatedNodes()
				for _, node := range allNodes {
					// Check if node has draining taint or is being deleted
					if node.DeletionTimestamp != nil {
						drainingNodes = append(drainingNodes, *node)
					}
					for _, taint := range node.Spec.Taints {
						if taint.Key == "karpenter.sh/disruption" {
							drainingNodes = append(drainingNodes, *node)
							break
						}
					}
				}

				// If we detect draining nodes, record this as a consolidation round
				if len(drainingNodes) > 0 {
					lastDrainingTime = time.Now()
					GinkgoWriter.Printf("   • Round %d: Detected %d nodes draining/terminating\n", roundNumber, len(drainingNodes))

					// Wait for this round to complete
					roundStartTime := time.Now()
					Eventually(func(g Gomega) {
						newNodeCount := env.Monitor.CreatedNodeCount()
						g.Expect(newNodeCount).To(BeNumerically("<", currentNodes), "Node count should decrease")
					}).WithTimeout(5 * time.Minute).Should(Succeed())

					finalNodeCount := env.Monitor.CreatedNodeCount()
					roundDuration := time.Since(roundStartTime)

					round := ConsolidationRound{
						RoundNumber:   roundNumber,
						StartTime:     roundStartTime,
						Duration:      roundDuration,
						NodesRemoved:  currentNodes - finalNodeCount,
						StartingNodes: currentNodes,
						EndingNodes:   finalNodeCount,
					}
					consolidationRounds = append(consolidationRounds, round)

					GinkgoWriter.Printf("   • Round %d completed: %d nodes removed in %v\n",
						roundNumber, round.NodesRemoved, roundDuration)
					roundNumber++
				}

				// Check for stability (no draining for 3 minutes)
				if time.Since(lastDrainingTime) >= 3*time.Minute {
					consolidationComplete = true
					GinkgoWriter.Printf("   • Consolidation complete: No draining activity for 3 minutes\n")
					break
				}

				// Wait before next check
				time.Sleep(30 * time.Second)
			}

			totalConsolidationTime := time.Since(lastDrainingTime)
			if !consolidationComplete {
				GinkgoWriter.Printf("   ⚠️  Consolidation timeout reached after %v\n", consolidationTimeout)
			}

			// Collect post-scale-in metrics
			By("Collecting post-scale-in metrics")
			postScaleInNodes := env.Monitor.CreatedNodeCount()
			postScaleInCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
			postScaleInMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)
			postScaleInEfficiencyScore := (postScaleInCPUUtil + postScaleInMemUtil) * 50

			// Update report with scale-in metrics
			report.ScaleInEnabled = true
			report.ScaleInPods = 700
			report.ConsolidationTime = totalConsolidationTime
			report.ConsolidationRounds = consolidationRounds
			report.PostScaleInNodes = postScaleInNodes
			report.PostScaleInCPUUtil = postScaleInCPUUtil
			report.PostScaleInMemoryUtil = postScaleInMemUtil
			report.PostScaleInEfficiencyScore = postScaleInEfficiencyScore

			// Report scale-in results
			GinkgoWriter.Printf("\n📊 SCALE-IN RESULTS:\n")
			GinkgoWriter.Printf("  • Pre-scale-in nodes: %d\n", preScaleInNodes)
			GinkgoWriter.Printf("  • Post-scale-in nodes: %d\n", postScaleInNodes)
			GinkgoWriter.Printf("  • Nodes consolidated: %d\n", preScaleInNodes-postScaleInNodes)
			GinkgoWriter.Printf("  • Consolidation rounds: %d\n", len(consolidationRounds))
			GinkgoWriter.Printf("  • Total consolidation time: %v\n", totalConsolidationTime)
			GinkgoWriter.Printf("  • Post-scale-in CPU utilization: %.2f%%\n", postScaleInCPUUtil*100)
			GinkgoWriter.Printf("  • Post-scale-in memory utilization: %.2f%%\n", postScaleInMemUtil*100)
			GinkgoWriter.Printf("  • Post-scale-in efficiency score: %.1f%%\n", postScaleInEfficiencyScore)

			// Add scale-in metrics to the final report if scale-in was performed
			if report.ScaleInEnabled {
				GinkgoWriter.Printf("\n🔽 SCALE-IN PERFORMANCE:\n")
				GinkgoWriter.Printf("  • Scale-in pods: %d\n", report.ScaleInPods)
				GinkgoWriter.Printf("  • Consolidation rounds: %d\n", len(report.ConsolidationRounds))
				GinkgoWriter.Printf("  • Total consolidation time: %v\n", report.ConsolidationTime)
				GinkgoWriter.Printf("  • Post-scale-in nodes: %d\n", report.PostScaleInNodes)
				GinkgoWriter.Printf("  • Post-scale-in CPU utilization: %.2f%%\n", report.PostScaleInCPUUtil*100)
				GinkgoWriter.Printf("  • Post-scale-in memory utilization: %.2f%%\n", report.PostScaleInMemoryUtil*100)
				GinkgoWriter.Printf("  • Post-scale-in efficiency score: %.1f%%\n", report.PostScaleInEfficiencyScore)

				if len(report.ConsolidationRounds) > 0 {
					GinkgoWriter.Printf("\n📊 CONSOLIDATION ROUNDS DETAIL:\n")
					for _, round := range report.ConsolidationRounds {
						GinkgoWriter.Printf("  • Round %d: %d nodes removed in %v (from %d to %d nodes)\n",
							round.RoundNumber, round.NodesRemoved, round.Duration,
							round.StartingNodes, round.EndingNodes)
					}
				}
			}

			// Write detailed report to file if OUTPUT_DIR is set
			if outputDir := os.Getenv("OUTPUT_DIR"); outputDir != "" {
				reportFile := filepath.Join(outputDir, "wide_deployments_performance_report.json")
				reportJSON, err := json.MarshalIndent(report, "", "  ")
				if err == nil {
					if err := os.WriteFile(reportFile, reportJSON, 0600); err == nil {
						GinkgoWriter.Printf("\n📄 Detailed JSON report written to: %s\n", reportFile)
					}
				}
			}

			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Scale-in assertions
			By("Validating scale-in performance")
			Expect(postScaleInNodes).To(BeNumerically("<", preScaleInNodes),
				"Node count should decrease after scale-in")

			Expect(totalConsolidationTime).To(BeNumerically("<", 15*time.Minute),
				"Consolidation should complete within 15 minutes")

			GinkgoWriter.Printf("✅ SCALE-IN TEST: Completed successfully\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			By("Performing comprehensive cleanup and verification")
			cleanupOpts := common.DefaultCleanupOptions()
			cleanupOpts.Deployments = deployments
			cleanupOpts.PodSelector = allPodsSelector
			cleanupOpts.WaitTimeout = 5 * time.Minute

			err := env.PerformComprehensiveCleanup(cleanupOpts)
			Expect(err).ToNot(HaveOccurred(), "Comprehensive cleanup should succeed")

			By("Performance test completed successfully")
		})
	})
})
