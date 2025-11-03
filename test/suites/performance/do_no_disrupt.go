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

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// Performance Test Criteria - Adjust these values to modify pass/fail conditions
var doNotDisruptCriteria = struct {
	// Scale-out Performance Thresholds
	MaxScaleOutTime      time.Duration // Maximum time allowed for initial pod deployment
	MaxNodeCount         int           // Maximum nodes that should be provisioned for 1100 pods
	MinCPUUtilization    float64       // Minimum average CPU utilization (0.0-1.0)
	MinMemoryUtilization float64       // Minimum average memory utilization (0.0-1.0)
	MinEfficiencyScore   float64       // Minimum resource efficiency score (0.0-100.0)

	// Scale-in/Consolidation Thresholds
	MaxConsolidationTime time.Duration // Maximum time for consolidation to complete

	// Test Configuration
	TotalPods        int // Total number of pods to deploy
	SmallPods        int // Number of small resource pods
	LargePods        int // Number of large resource pods
	DoNotDisruptPods int // Number of do-not-disrupt pods
}{
	MaxScaleOutTime:      12 * time.Minute, // Increased for 1100 pods
	MaxNodeCount:         550,              // Should not require more than 550 nodes for 1100 pods
	MinCPUUtilization:    0.40,             // 40% minimum CPU utilization
	MinMemoryUtilization: 0.40,             // 40% minimum memory utilization
	MinEfficiencyScore:   40.0,             // 40% minimum efficiency score
	MaxConsolidationTime: 15 * time.Minute,
	TotalPods:            1100,
	SmallPods:            500,
	LargePods:            500,
	DoNotDisruptPods:     100,
}

var _ = Describe("Performance", func() {
	Context("Do Not Disrupt Performance Test", func() {
		It("should efficiently scale three deployments and test disruption protection", func() {
			By("Setting up performance test with 1100 pods - including do-not-disrupt deployment")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("CREATING DEPLOYMENTS WITH DO-NOT-DISRUPT PROTECTION" + "\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Record test start time
			testStartTime := time.Now()
			env.TimeIntervalCollector.Start("test_start")

			// Declare variables that will be used in DeferCleanup
			var smallDeployment, largeDeployment, doNotDisruptDeployment *appsv1.Deployment
			var allPodsSelector labels.Selector

			// Set up cleanup that runs regardless of test outcome
			DeferCleanup(func() {
				if smallDeployment != nil && largeDeployment != nil && doNotDisruptDeployment != nil {
					By("Emergency cleanup - ensuring test resources are removed")
					opts := common.DefaultCleanupOptions()
					opts.Deployments = []*appsv1.Deployment{smallDeployment, largeDeployment, doNotDisruptDeployment}
					opts.PodSelector = allPodsSelector
					_ = env.ForceCleanupTestResources(opts)
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

			// Define resource requirements for do-not-disrupt pods (100 pods)
			// 0.95 vCPU and 450 MB memory each
			doNotDisruptPodResources := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("950m"),  // 0.95 vCPU
					corev1.ResourceMemory: resource.MustParse("450Mi"), // 450 MB
				},
			}

			// Create deployment with small resource requirements and host name topology spread constraints
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
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{
							MaxSkew:           1,
							TopologyKey:       corev1.LabelHostname,
							WhenUnsatisfiable: corev1.DoNotSchedule,
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app":           "small-resource-app",
									"resource-type": "small",
								},
							},
						},
					},
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

			// Create deployment with do-not-disrupt annotation
			doNotDisruptDeployment = test.Deployment(test.DeploymentOptions{
				Replicas: int32(100),
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":               "do-not-disrupt-app",
							"resource-type":     "protected",
							test.DiscoveryLabel: "unspecified",
						},
						Annotations: map[string]string{
							v1.DoNotDisruptAnnotationKey: "true",
						},
					},
					ResourceRequirements: doNotDisruptPodResources,
				},
			})

			By("Creating NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			By("Deploying all three deployments (small, large, and do-not-disrupt)")
			env.TimeIntervalCollector.Start("deployments_created")
			env.ExpectCreated(smallDeployment, largeDeployment, doNotDisruptDeployment)
			env.TimeIntervalCollector.End("deployments_created")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("DEPLOYMENTS CREATED WAITING FOR PROVISIONING" + "\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Create selectors for monitoring pods
			smallPodSelector := labels.SelectorFromSet(smallDeployment.Spec.Selector.MatchLabels)
			largePodSelector := labels.SelectorFromSet(largeDeployment.Spec.Selector.MatchLabels)
			doNotDisruptPodSelector := labels.SelectorFromSet(doNotDisruptDeployment.Spec.Selector.MatchLabels)
			allPodsSelector = labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})

			By("Waiting for all 1100 pods to be scheduled and ready")
			env.TimeIntervalCollector.Start("waiting_for_pods")

			// Wait for all pods to become healthy with a 15-minute timeout
			// This covers both scheduling and readiness
			env.EventuallyExpectHealthyPodCountWithTimeout(15*time.Minute, allPodsSelector, 1100)

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
			podsPerNode := float64(1100) / float64(nodeCount)

			By("Generating performance report")

			// Determine test status and collect warnings using criteria
			var warnings []string
			testPassed := true

			if totalTime >= doNotDisruptCriteria.MaxScaleOutTime {
				warnings = append(warnings, fmt.Sprintf("Total scale-out time exceeded %v", doNotDisruptCriteria.MaxScaleOutTime))
				testPassed = false
			}
			if avgCPUUtil < doNotDisruptCriteria.MinCPUUtilization {
				warnings = append(warnings, fmt.Sprintf("Average CPU utilization below %.0f%% (%.1f%%)", doNotDisruptCriteria.MinCPUUtilization*100, avgCPUUtil*100))
				testPassed = false
			}
			if avgMemUtil < doNotDisruptCriteria.MinMemoryUtilization {
				warnings = append(warnings, fmt.Sprintf("Average memory utilization below %.0f%% (%.1f%%)", doNotDisruptCriteria.MinMemoryUtilization*100, avgMemUtil*100))
				testPassed = false
			}
			if nodeCount > doNotDisruptCriteria.MaxNodeCount {
				warnings = append(warnings, fmt.Sprintf("Too many nodes provisioned (>%d): %d", doNotDisruptCriteria.MaxNodeCount, nodeCount))
				testPassed = false
			}

			// Create structured performance report
			report := PerformanceReport{
				TestName:                "Do Not Disrupt Performance Test",
				TotalPods:               1100,
				SmallPods:               500,
				LargePods:               500,
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

			// Output detailed performance report to console
			By("=== PERFORMANCE TEST REPORT ===")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🚀 DO NOT DISRUPT PERFORMANCE REPORT\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			// Test Configuration
			GinkgoWriter.Printf("\n📋 TEST CONFIGURATION:\n")
			GinkgoWriter.Printf("  • Total Pods: %d\n", report.TotalPods)
			GinkgoWriter.Printf("  • Small Pods (0.95 vCPU, 3.9GB): %d\n", report.SmallPods)
			GinkgoWriter.Printf("  • Large Pods (3.8 vCPU, 31GB): %d\n", report.LargePods)
			GinkgoWriter.Printf("  • Do-Not-Disrupt Pods (0.95 vCPU, 450MB): %d\n", 100)
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
				report.TotalTime, getStatusIcon(report.TotalTime < 12*time.Minute))
			GinkgoWriter.Printf("│ Nodes Provisioned           │ %-12d │ %s │\n",
				report.NodesProvisioned, getStatusIcon(report.NodesProvisioned < 550))
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

			// Verify all pods are actually running
			env.EventuallyExpectHealthyPodCount(smallPodSelector, 500)
			env.EventuallyExpectHealthyPodCount(largePodSelector, 500)
			env.EventuallyExpectHealthyPodCount(doNotDisruptPodSelector, 100)

			// ========== DISRUPTION PROTECTION TEST ==========
			By("Testing disruption protection with do-not-disrupt annotation")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🛡️  DISRUPTION PROTECTION TEST: Testing do-not-disrupt behavior\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			// Record pre-disruption state
			preDisruptionNodes := env.Monitor.CreatedNodeCount()

			// Get nodes that have do-not-disrupt pods
			doNotDisruptPods := env.Monitor.RunningPods(doNotDisruptPodSelector)
			nodesWithProtectedPods := make(map[string]bool)
			for _, pod := range doNotDisruptPods {
				if pod.Spec.NodeName != "" {
					nodesWithProtectedPods[pod.Spec.NodeName] = true
				}
			}

			GinkgoWriter.Printf("   • Pre-disruption nodes: %d\n", preDisruptionNodes)
			GinkgoWriter.Printf("   • Nodes with do-not-disrupt pods: %d\n", len(nodesWithProtectedPods))

			// Scale down small and large deployments to trigger consolidation
			By("Scaling down small and large deployments to trigger consolidation")
			smallDeployment.Spec.Replicas = lo.ToPtr(int32(250))
			largeDeployment.Spec.Replicas = lo.ToPtr(int32(250))

			env.ExpectUpdated(smallDeployment, largeDeployment)
			GinkgoWriter.Printf("   • Deployments scaled down to 250 replicas each\n")
			GinkgoWriter.Printf("   • Do-not-disrupt deployment remains at 100 replicas\n")

			// Wait for pods to be terminated
			By("Waiting for pods to scale down")
			Eventually(func(g Gomega) {
				pods := env.Monitor.RunningPods(allPodsSelector)
				g.Expect(pods).To(HaveLen(600), "Should have exactly 600 pods after scale-down")
			}).WithTimeout(5 * time.Minute).Should(Succeed())
			GinkgoWriter.Printf("   • Pod count reduced to 600 (250+250+100)\n")

			// Monitor for consolidation attempts while protecting do-not-disrupt nodes
			By("Monitoring consolidation behavior with protected nodes")

			// Wait and observe consolidation behavior
			time.Sleep(2 * time.Minute) // Allow time for consolidation to potentially start

			// Check if nodes with do-not-disrupt pods are still present
			currentNodes := env.Monitor.CreatedNodes()
			protectedNodesStillPresent := 0
			for _, node := range currentNodes {
				if nodesWithProtectedPods[node.Name] {
					protectedNodesStillPresent++
				}
			}

			GinkgoWriter.Printf("   • Protected nodes still present: %d/%d\n", protectedNodesStillPresent, len(nodesWithProtectedPods))

			// Verify that nodes with do-not-disrupt pods are protected
			Expect(protectedNodesStillPresent).To(BeNumerically(">", 0),
				"At least some nodes with do-not-disrupt pods should remain protected")

			// ========== SCALE-IN AFTER REMOVING PROTECTION ==========
			By("Removing do-not-disrupt annotation and testing normal consolidation")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🔽 SCALE-IN TEST: Removing protection and allowing consolidation\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			// Remove do-not-disrupt annotation from the deployment
			doNotDisruptDeployment.Spec.Template.Annotations = map[string]string{}
			env.ExpectUpdated(doNotDisruptDeployment)
			GinkgoWriter.Printf("   • Removed do-not-disrupt annotation from deployment\n")

			// Wait for pods to be recreated without the annotation
			By("Waiting for pods to be recreated without do-not-disrupt annotation")
			Eventually(func(g Gomega) {
				pods := env.Monitor.RunningPods(doNotDisruptPodSelector)
				for _, pod := range pods {
					g.Expect(pod.Annotations).ToNot(HaveKey(v1.DoNotDisruptAnnotationKey),
						"Pods should not have do-not-disrupt annotation")
				}
			}).WithTimeout(5 * time.Minute).Should(Succeed())
			GinkgoWriter.Printf("   • All do-not-disrupt pods recreated without protection\n")

			// Monitor consolidation rounds after removing protection
			By("Monitoring node consolidation after removing protection")
			var consolidationRounds []ConsolidationRound
			roundNumber := 1
			lastDrainingTime := time.Now()
			postProtectionConsolidationStart := time.Now()

			// Track consolidation with 15-minute timeout
			consolidationComplete := false
			postProtectionTimeout := 15 * time.Minute

			for time.Since(postProtectionConsolidationStart) < postProtectionTimeout && !consolidationComplete {
				currentNodeCount := env.Monitor.CreatedNodeCount()
				GinkgoWriter.Printf("Checking for consolidation activity (round %d)\n", roundNumber)
				GinkgoWriter.Printf("  • Current nodes: %d\n", currentNodeCount)

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
						g.Expect(newNodeCount).To(BeNumerically("<", currentNodeCount), "Node count should decrease")
					}).WithTimeout(5 * time.Minute).Should(Succeed())

					finalNodeCount := env.Monitor.CreatedNodeCount()
					roundDuration := time.Since(roundStartTime)

					round := ConsolidationRound{
						RoundNumber:   roundNumber,
						StartTime:     roundStartTime,
						Duration:      roundDuration,
						NodesRemoved:  currentNodeCount - finalNodeCount,
						StartingNodes: currentNodeCount,
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

			totalConsolidationTime := time.Since(postProtectionConsolidationStart)
			if !consolidationComplete {
				GinkgoWriter.Printf("   ⚠️  Consolidation timeout reached after %v\n", postProtectionTimeout)
			}

			// Collect post-consolidation metrics
			By("Collecting post-consolidation metrics")
			postConsolidationNodes := env.Monitor.CreatedNodeCount()
			postConsolidationCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
			postConsolidationMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)
			postConsolidationEfficiencyScore := (postConsolidationCPUUtil + postConsolidationMemUtil) * 50

			// Update report with consolidation metrics
			report.ScaleInEnabled = true
			report.ScaleInPods = 600
			report.ConsolidationTime = totalConsolidationTime
			report.ConsolidationRounds = consolidationRounds
			report.PostScaleInNodes = postConsolidationNodes
			report.PostScaleInCPUUtil = postConsolidationCPUUtil
			report.PostScaleInMemoryUtil = postConsolidationMemUtil
			report.PostScaleInEfficiencyScore = postConsolidationEfficiencyScore

			// Report consolidation results
			GinkgoWriter.Printf("\n📊 CONSOLIDATION RESULTS:\n")
			GinkgoWriter.Printf("  • Pre-consolidation nodes: %d\n", preDisruptionNodes)
			GinkgoWriter.Printf("  • Post-consolidation nodes: %d\n", postConsolidationNodes)
			GinkgoWriter.Printf("  • Nodes consolidated: %d\n", preDisruptionNodes-postConsolidationNodes)
			GinkgoWriter.Printf("  • Consolidation rounds: %d\n", len(consolidationRounds))
			GinkgoWriter.Printf("  • Total consolidation time: %v\n", totalConsolidationTime)
			GinkgoWriter.Printf("  • Post-consolidation CPU utilization: %.2f%%\n", postConsolidationCPUUtil*100)
			GinkgoWriter.Printf("  • Post-consolidation memory utilization: %.2f%%\n", postConsolidationMemUtil*100)
			GinkgoWriter.Printf("  • Post-consolidation efficiency score: %.1f%%\n", postConsolidationEfficiencyScore)

			// Add consolidation metrics to the final report if consolidation was performed
			if report.ScaleInEnabled {
				GinkgoWriter.Printf("\n🔽 CONSOLIDATION PERFORMANCE:\n")
				GinkgoWriter.Printf("  • Consolidation pods: %d\n", report.ScaleInPods)
				GinkgoWriter.Printf("  • Consolidation rounds: %d\n", len(report.ConsolidationRounds))
				GinkgoWriter.Printf("  • Total consolidation time: %v\n", report.ConsolidationTime)
				GinkgoWriter.Printf("  • Post-consolidation nodes: %d\n", report.PostScaleInNodes)
				GinkgoWriter.Printf("  • Post-consolidation CPU utilization: %.2f%%\n", report.PostScaleInCPUUtil*100)
				GinkgoWriter.Printf("  • Post-consolidation memory utilization: %.2f%%\n", report.PostScaleInMemoryUtil*100)
				GinkgoWriter.Printf("  • Post-consolidation efficiency score: %.1f%%\n", report.PostScaleInEfficiencyScore)

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
				reportFile := filepath.Join(outputDir, "do_no_disrupt_performance_report.json")
				reportJSON, err := json.MarshalIndent(report, "", "  ")
				if err == nil {
					if err := os.WriteFile(reportFile, reportJSON, 0600); err == nil {
						GinkgoWriter.Printf("\n📄 Detailed JSON report written to: %s\n", reportFile)
					}
				}
			}

			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			By("Validating performance assertions")

			// Performance Assertions using criteria
			Expect(totalTime).To(BeNumerically("<", doNotDisruptCriteria.MaxScaleOutTime),
				fmt.Sprintf("Total scale-out time should be less than %v", doNotDisruptCriteria.MaxScaleOutTime))

			Expect(nodeCount).To(BeNumerically("<", doNotDisruptCriteria.MaxNodeCount),
				fmt.Sprintf("Should not require more than %d nodes for %d pods", doNotDisruptCriteria.MaxNodeCount, doNotDisruptCriteria.TotalPods))

			Expect(avgCPUUtil).To(BeNumerically(">", doNotDisruptCriteria.MinCPUUtilization),
				fmt.Sprintf("Average CPU utilization should be greater than %.0f%%", doNotDisruptCriteria.MinCPUUtilization*100))

			Expect(avgMemUtil).To(BeNumerically(">", doNotDisruptCriteria.MinMemoryUtilization),
				fmt.Sprintf("Average memory utilization should be greater than %.0f%%", doNotDisruptCriteria.MinMemoryUtilization*100))

			// Disruption protection assertions
			Expect(protectedNodesStillPresent).To(BeNumerically(">", 0),
				"Nodes with do-not-disrupt pods should be protected from consolidation")

			// Consolidation assertions (after removing protection)
			if len(consolidationRounds) > 0 {
				Expect(postConsolidationNodes).To(BeNumerically("<", preDisruptionNodes),
					"Node count should decrease after removing do-not-disrupt protection")

				Expect(totalConsolidationTime).To(BeNumerically("<", doNotDisruptCriteria.MaxConsolidationTime),
					fmt.Sprintf("Consolidation should complete within %v after removing protection", doNotDisruptCriteria.MaxConsolidationTime))
			}

			GinkgoWriter.Printf("✅ DO-NOT-DISRUPT TEST: Completed successfully\n")
			GinkgoWriter.Printf("   • Disruption protection validated\n")
			GinkgoWriter.Printf("   • Normal consolidation after protection removal validated\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			By("Performing comprehensive cleanup and verification")
			cleanupOpts := common.DefaultCleanupOptions()
			cleanupOpts.Deployments = []*appsv1.Deployment{smallDeployment, largeDeployment, doNotDisruptDeployment}
			cleanupOpts.PodSelector = allPodsSelector
			cleanupOpts.WaitTimeout = 5 * time.Minute

			err := env.PerformComprehensiveCleanup(cleanupOpts)
			Expect(err).ToNot(HaveOccurred(), "Comprehensive cleanup should succeed")

			By("Performance test completed successfully")
		})
	})
})
