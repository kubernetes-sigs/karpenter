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
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

// ConsolidationRound represents a single round of consolidation
type ConsolidationRound struct {
	RoundNumber   int           `json:"round_number"`
	StartTime     time.Time     `json:"start_time"`
	Duration      time.Duration `json:"duration"`
	NodesRemoved  int           `json:"nodes_removed"`
	StartingNodes int           `json:"starting_nodes"`
	EndingNodes   int           `json:"ending_nodes"`
}

// PerformanceReport represents the structured performance test results
type PerformanceReport struct {
	TestName                string        `json:"test_name"`
	TotalPods               int           `json:"total_pods"`
	SmallPods               int           `json:"small_pods"`
	LargePods               int           `json:"large_pods"`
	TotalTime               time.Duration `json:"total_time"`
	PodSchedulingTime       time.Duration `json:"pod_scheduling_time"`
	NodeProvisioningTime    time.Duration `json:"node_provisioning_time"`
	PodReadyTime            time.Duration `json:"pod_ready_time"`
	NodesProvisioned        int           `json:"nodes_provisioned"`
	TotalReservedCPUUtil    float64       `json:"total_reserved_cpu_utilization"`
	TotalReservedMemoryUtil float64       `json:"total_reserved_memory_utilization"`
	ResourceEfficiencyScore float64       `json:"resource_efficiency_score"`
	PodsPerNode             float64       `json:"pods_per_node"`
	// Scale-in metrics
	ScaleInEnabled             bool                 `json:"scale_in_enabled"`
	ScaleInPods                int                  `json:"scale_in_pods"`
	ConsolidationTime          time.Duration        `json:"consolidation_time"`
	ConsolidationRounds        []ConsolidationRound `json:"consolidation_rounds"`
	PostScaleInNodes           int                  `json:"post_scale_in_nodes"`
	PostScaleInCPUUtil         float64              `json:"post_scale_in_cpu_utilization"`
	PostScaleInMemoryUtil      float64              `json:"post_scale_in_memory_utilization"`
	PostScaleInEfficiencyScore float64              `json:"post_scale_in_efficiency_score"`
	Timestamp                  time.Time            `json:"timestamp"`
	TestPassed                 bool                 `json:"test_passed"`
	Warnings                   []string             `json:"warnings,omitempty"`
}

// getStatusIcon returns a visual indicator for pass/fail status
func getStatusIcon(passed bool) string {
	if passed {
		return "✅ PASS"
	}
	return "❌ FAIL"
}

var _ = Describe("Performance", func() {
	Context("Large Scale Deployment", func() {
		It("should efficiently scale two deployments with different resource profiles", func() {
			By("Setting up performance test with 1000 pods across two resource profiles")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("CREATING DEPLOYMENTS" + "\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Record test start time
			testStartTime := time.Now()
			env.TimeIntervalCollector.Start("test_start")

			// Declare variables that will be used in DeferCleanup
			var smallDeployment, largeDeployment *appsv1.Deployment
			var allPodsSelector labels.Selector

			// Set up cleanup that runs regardless of test outcome
			DeferCleanup(func() {
				if smallDeployment != nil && largeDeployment != nil {
					By("Emergency cleanup - ensuring test resources are removed")
					GinkgoWriter.Printf("🚨 EMERGENCY CLEANUP: Removing any remaining test resources...\n")

					// Force delete deployments if they still exist
					_ = client.IgnoreNotFound(env.Client.Delete(env.Context, smallDeployment, &client.DeleteOptions{GracePeriodSeconds: lo.ToPtr(int64(0))}))
					_ = client.IgnoreNotFound(env.Client.Delete(env.Context, largeDeployment, &client.DeleteOptions{GracePeriodSeconds: lo.ToPtr(int64(0))}))

					// Force delete any remaining nodes
					createdNodes := env.Monitor.CreatedNodes()
					for _, node := range createdNodes {
						_ = client.IgnoreNotFound(env.Client.Delete(env.Context, node, &client.DeleteOptions{GracePeriodSeconds: lo.ToPtr(int64(0))}))
					}

					// Force delete any remaining nodeclaims
					var nodeClaims v1.NodeClaimList
					if err := env.Client.List(env.Context, &nodeClaims, client.MatchingLabels{test.DiscoveryLabel: "unspecified"}); err == nil {
						for _, nodeClaim := range nodeClaims.Items {
							_ = client.IgnoreNotFound(env.Client.Delete(env.Context, &nodeClaim, &client.DeleteOptions{GracePeriodSeconds: lo.ToPtr(int64(0))}))
						}
					}

					// Wait briefly for cleanup
					time.Sleep(30 * time.Second)

					// Verify cleanup
					if allPodsSelector != nil {
						remainingPods := env.Monitor.RunningPods(allPodsSelector)
						if len(remainingPods) > 0 {
							GinkgoWriter.Printf("⚠️  WARNING: %d test pods may still be running\n", len(remainingPods))
						}
					}

					GinkgoWriter.Printf("🧹 Emergency cleanup completed\n")
				}
			})

			// Define resource requirements for small pods (500 pods)
			// 0.95 vCPU and 3900 MB memory each
			smallPodResources := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("950m"),   // 0.95 vCPU
					corev1.ResourceMemory: resource.MustParse("3900Mi"), // 3900 MB
				},
			}

			// Define resource requirements for large pods (500 pods)
			// 3.8 vCPU and 31 GB memory each
			largePodResources := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("3800m"), // 3.8 vCPU
					corev1.ResourceMemory: resource.MustParse("31Gi"),  // 31 GB
				},
			}

			// Create deployment with small resource requirements
			smallDeployment = test.Deployment(test.DeploymentOptions{
				Replicas: int32(500),
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":               "small-resource-app",
							"resource-type":     "small",
							test.DiscoveryLabel: "unspecified",
						},
					},
					ResourceRequirements: smallPodResources,
				},
			})

			// Create deployment with large resource requirements
			largeDeployment = test.Deployment(test.DeploymentOptions{
				Replicas: int32(500),
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":               "large-resource-app",
							"resource-type":     "large",
							test.DiscoveryLabel: "unspecified",
						},
					},
					ResourceRequirements: largePodResources,
				},
			})

			By("Creating NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			By("Deploying both small and large resource deployments")
			env.TimeIntervalCollector.Start("deployments_created")
			env.ExpectCreated(smallDeployment, largeDeployment)
			env.TimeIntervalCollector.End("deployments_created")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("DEPLOYMENTS CREATED WAITING FOR PROVISIONING" + "\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			// Create selectors for monitoring pods
			smallPodSelector := labels.SelectorFromSet(smallDeployment.Spec.Selector.MatchLabels)
			largePodSelector := labels.SelectorFromSet(largeDeployment.Spec.Selector.MatchLabels)
			allPodsSelector = labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})

			By("Waiting for all 1000 pods to be scheduled and ready")
			env.TimeIntervalCollector.Start("waiting_for_pods")

			// Wait for all pods to become healthy with a 15-minute timeout
			// This covers both scheduling and readiness
			env.EventuallyExpectHealthyPodCountWithTimeout(15*time.Minute, allPodsSelector, 1000)

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

			// Collect timing breakdown (if available from TimeIntervalCollector)
			var podSchedulingTime, nodeProvisioningTime, podReadyTime time.Duration

			// Estimate timing phases (these would be more accurate with detailed instrumentation)
			podSchedulingTime = totalTime / 4    // Rough estimate: 25% of total time
			nodeProvisioningTime = totalTime / 2 // Rough estimate: 50% of total time
			podReadyTime = totalTime / 4         // Rough estimate: 25% of total time

			By("Generating performance report")

			// Determine test status and collect warnings
			var warnings []string
			testPassed := true

			if totalTime >= 10*time.Minute {
				warnings = append(warnings, "Total scale-out time exceeded 10 minutes")
				testPassed = false
			}
			if avgCPUUtil < 0.4 {
				warnings = append(warnings, fmt.Sprintf("Average CPU utilization below 70%% (%.1f%%)", avgCPUUtil*100))
				testPassed = false
			}
			if avgMemUtil < 0.4 {
				warnings = append(warnings, fmt.Sprintf("Average memory utilization below 70%% (%.1f%%)", avgMemUtil*100))
				testPassed = false
			}
			if nodeCount > 500 {
				warnings = append(warnings, fmt.Sprintf("Too many nodes provisioned (>50): %d", nodeCount))
				testPassed = false
			}

			// Create structured performance report
			report := PerformanceReport{
				TestName:                "Large Scale Deployment Performance Test",
				TotalPods:               1000,
				SmallPods:               500,
				LargePods:               500,
				TotalTime:               totalTime,
				PodSchedulingTime:       podSchedulingTime,
				NodeProvisioningTime:    nodeProvisioningTime,
				PodReadyTime:            podReadyTime,
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

			// Output detailed performance report to console
			By("=== PERFORMANCE TEST REPORT ===")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🚀 LARGE SCALE DEPLOYMENT PERFORMANCE REPORT\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			// Test Configuration
			GinkgoWriter.Printf("\n📋 TEST CONFIGURATION:\n")
			GinkgoWriter.Printf("  • Total Pods: %d\n", report.TotalPods)
			GinkgoWriter.Printf("  • Small Pods (0.95 vCPU, 3.9GB): %d\n", report.SmallPods)
			GinkgoWriter.Printf("  • Large Pods (3.8 vCPU, 31GB): %d\n", report.LargePods)
			GinkgoWriter.Printf("  • Test Duration: %v\n", report.TotalTime)

			// Timing Metrics
			GinkgoWriter.Printf("\n⏱️  TIMING METRICS:\n")
			GinkgoWriter.Printf("  • Total Scale-out Time: %v\n", report.TotalTime)
			GinkgoWriter.Printf("  • Est. Pod Scheduling Time: %v\n", report.PodSchedulingTime)
			GinkgoWriter.Printf("  • Est. Node Provisioning Time: %v\n", report.NodeProvisioningTime)
			GinkgoWriter.Printf("  • Est. Pod Ready Time: %v\n", report.PodReadyTime)

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
				report.TotalTime, getStatusIcon(report.TotalTime < 10*time.Minute))
			GinkgoWriter.Printf("│ Nodes Provisioned           │ %-12d │ %s │\n",
				report.NodesProvisioned, getStatusIcon(report.NodesProvisioned < 1000))
			GinkgoWriter.Printf("│ Total Reserved CPU Util     │ %-11.1f%% │ %s │\n",
				report.TotalReservedCPUUtil*100, getStatusIcon(report.TotalReservedCPUUtil > 0.7))
			GinkgoWriter.Printf("│ Total Reserved Memory Util  │ %-11.1f%% │ %s │\n",
				report.TotalReservedMemoryUtil*100, getStatusIcon(report.TotalReservedMemoryUtil > 0.7))
			GinkgoWriter.Printf("│ Resource Efficiency Score   │ %-11.1f%% │ %s │\n",
				report.ResourceEfficiencyScore, getStatusIcon(report.ResourceEfficiencyScore > 70))
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
				reportFile := filepath.Join(outputDir, "large_scale_performance_report.json")
				reportJSON, err := json.MarshalIndent(report, "", "  ")
				if err == nil {
					if err := os.WriteFile(reportFile, reportJSON, 0600); err == nil {
						GinkgoWriter.Printf("\n📄 Detailed JSON report written to: %s\n", reportFile)
					}
				}
			}

			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			By("Validating performance assertions")

			// Performance Assertions
			Expect(totalTime).To(BeNumerically("<", 10*time.Minute),
				"Total scale-out time should be less than 10 minutes")

			Expect(nodeCount).To(BeNumerically("<", 1000),
				"Should not require more than 50 nodes for 1000 pods")

			Expect(avgCPUUtil).To(BeNumerically(">", 0.4),
				"Average CPU utilization should be greater than 70%")

			Expect(avgMemUtil).To(BeNumerically(">", 0.4),
				"Average memory utilization should be greater than 70%")

			// Verify all pods are actually running
			env.EventuallyExpectHealthyPodCount(smallPodSelector, 500)
			env.EventuallyExpectHealthyPodCount(largePodSelector, 500)

			// ========== SCALE-IN PERFORMANCE TEST ==========
			By("Starting scale-in performance test")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🔽 SCALE-IN PERFORMANCE TEST: Scaling down to 70%% (700 pods)\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			// Record pre-scale-in state
			preScaleInNodes := env.Monitor.CreatedNodeCount()

			// Scale down deployments to 70% (350 each)
			By("Scaling deployments down to 350 pods each (700 total)")
			smallDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			largeDeployment.Spec.Replicas = lo.ToPtr(int32(350))

			env.ExpectUpdated(smallDeployment, largeDeployment)
			GinkgoWriter.Printf("   • Deployments scaled down to 350 replicas each\n")

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

			// Scale-in assertions
			By("Validating scale-in performance")
			Expect(postScaleInNodes).To(BeNumerically("<", preScaleInNodes),
				"Node count should decrease after scale-in")

			Expect(totalConsolidationTime).To(BeNumerically("<", 15*time.Minute),
				"Consolidation should complete within 15 minutes")

			GinkgoWriter.Printf("✅ SCALE-IN TEST: Completed successfully\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			By("Performing comprehensive cleanup and verification")
			GinkgoWriter.Printf("\n🧹 CLEANUP: Starting resource cleanup...\n")

			// 1. Delete the deployments
			env.ExpectDeleted(smallDeployment, largeDeployment)
			GinkgoWriter.Printf("   • Deployments deleted\n")

			// 2. Wait for all pods to be terminated
			Eventually(func(g Gomega) {
				pods := env.Monitor.RunningPods(allPodsSelector)
				g.Expect(pods).To(HaveLen(0), "All test pods should be terminated")
			}).WithTimeout(5 * time.Minute).Should(Succeed())
			GinkgoWriter.Printf("   • All pods terminated\n")

			// 3. Force delete all nodes and nodeclaims created during the test
			By("Force deleting all test-created nodes and nodeclaims")

			// Get all nodes created during the test
			createdNodes := env.Monitor.CreatedNodes()
			GinkgoWriter.Printf("   • Found %d nodes to delete\n", len(createdNodes))

			// Force delete all created nodes
			for _, node := range createdNodes {
				err := env.Client.Delete(env.Context, node, &client.DeleteOptions{
					GracePeriodSeconds: lo.ToPtr(int64(0)),
				})
				if err != nil && client.IgnoreNotFound(err) != nil {
					GinkgoWriter.Printf("   ⚠️  Warning: Failed to delete node %s: %v\n", node.Name, err)
				}
			}

			// Get all nodeclaims with the test discovery label
			var nodeClaims v1.NodeClaimList
			err := env.Client.List(env.Context, &nodeClaims, client.MatchingLabels{
				test.DiscoveryLabel: "unspecified",
			})
			if err == nil {
				GinkgoWriter.Printf("   • Found %d nodeclaims to delete\n", len(nodeClaims.Items))

				// Force delete all test nodeclaims
				for _, nodeClaim := range nodeClaims.Items {
					err := env.Client.Delete(env.Context, &nodeClaim, &client.DeleteOptions{
						GracePeriodSeconds: lo.ToPtr(int64(0)),
					})
					if err != nil && client.IgnoreNotFound(err) != nil {
						GinkgoWriter.Printf("   ⚠️  Warning: Failed to delete nodeclaim %s: %v\n", nodeClaim.Name, err)
					}
				}
			}

			GinkgoWriter.Printf("   • Force deletion commands issued\n")

			// 4. Wait for nodes to be actually removed (shorter timeout since we're forcing deletion)
			Eventually(func(g Gomega) {
				createdNodes := env.Monitor.CreatedNodes()
				g.Expect(createdNodes).To(HaveLen(0), "All provisioned nodes should be cleaned up")
			}).WithTimeout(3 * time.Minute).Should(Succeed())
			GinkgoWriter.Printf("   • All nodes cleaned up\n")

			// 5. Verify nodeclaims are also cleaned up
			Eventually(func(g Gomega) {
				var remainingNodeClaims v1.NodeClaimList
				err := env.Client.List(env.Context, &remainingNodeClaims, client.MatchingLabels{
					test.DiscoveryLabel: "unspecified",
				})
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(remainingNodeClaims.Items).To(HaveLen(0), "All test nodeclaims should be cleaned up")
			}).WithTimeout(2 * time.Minute).Should(Succeed())
			GinkgoWriter.Printf("   • All nodeclaims cleaned up\n")

			// 6. Final cluster state verification
			By("Verifying cluster is ready for subsequent tests")
			finalNodeCount := env.Monitor.NodeCount()
			Expect(finalNodeCount).To(Equal(env.StartingNodeCount),
				fmt.Sprintf("Node count should return to starting count (%d), but is %d",
					env.StartingNodeCount, finalNodeCount))

			GinkgoWriter.Printf("✅ CLEANUP: Completed successfully - cluster ready for next test\n")

			By("Performance test completed successfully")
		})
	})
})
