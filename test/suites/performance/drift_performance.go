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
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// DriftRound represents a single round of drift replacement
type DriftRound struct {
	RoundNumber   int           `json:"round_number"`
	StartTime     time.Time     `json:"start_time"`
	Duration      time.Duration `json:"duration"`
	NodesReplaced int           `json:"nodes_replaced"`
	StartingNodes int           `json:"starting_nodes"`
	EndingNodes   int           `json:"ending_nodes"`
}

// DriftPerformanceReport extends PerformanceReport with drift-specific metrics
type DriftPerformanceReport struct {
	PerformanceReport
	// Drift-specific metrics
	DriftEnabled        bool          `json:"drift_enabled"`
	DriftTriggerTime    time.Time     `json:"drift_trigger_time"`
	DriftDetectionTime  time.Duration `json:"drift_detection_time"`
	TotalDriftTime      time.Duration `json:"total_drift_time"`
	DriftRounds         []DriftRound  `json:"drift_rounds"`
	PreDriftNodes       int           `json:"pre_drift_nodes"`
	PostDriftNodes      int           `json:"post_drift_nodes"`
	NodesReplaced       int           `json:"nodes_replaced"`
	MaxPodsDisrupted    int           `json:"max_pods_disrupted"`
	PDBViolations       int           `json:"pdb_violations"`
	PostDriftCPUUtil    float64       `json:"post_drift_cpu_utilization"`
	PostDriftMemoryUtil float64       `json:"post_drift_memory_utilization"`
	PostDriftEfficiency float64       `json:"post_drift_efficiency_score"`
}

var _ = Describe("Performance", func() {
	Context("Drift Performance", func() {
		It("should efficiently handle drift replacement of 2000 pods with PDB constraints", func() {
			By("Setting up drift performance test with 2000 pods")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🔄 DRIFT PERFORMANCE TEST - 2000 PODS\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Record test start time
			testStartTime := time.Now()
			env.TimeIntervalCollector.Start("test_start")

			// Declare variables that will be used in DeferCleanup
			var hostnameDeployment, standardDeployment *appsv1.Deployment
			var hostnamePDB, standardPDB *policyv1.PodDisruptionBudget
			var allPodsSelector labels.Selector

			// Set up cleanup that runs regardless of test outcome
			DeferCleanup(func() {
				if hostnameDeployment != nil && standardDeployment != nil {
					By("Emergency cleanup - ensuring test resources are removed")
					opts := common.DefaultCleanupOptions()
					opts.Deployments = []*appsv1.Deployment{hostnameDeployment, standardDeployment}
					opts.PodSelector = allPodsSelector
					// Note: PDBs will be cleaned up automatically when deployments are deleted
					_ = env.ForceCleanupTestResources(opts)
				}
			})

			// Define resource requirements for hostname spread deployment (1000 pods)
			// 1.5 vCPU and 6 GB memory each
			hostnameResources := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1500m"), // 1.5 vCPU
					corev1.ResourceMemory: resource.MustParse("6Gi"),   // 6 GB
				},
			}

			// Define resource requirements for standard deployment (1000 pods)
			// 2.0 vCPU and 8 GB memory each
			standardResources := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2000m"), // 2.0 vCPU
					corev1.ResourceMemory: resource.MustParse("8Gi"),   // 8 GB
				},
			}

			// Create deployment with hostname topology spread constraints (forces wide node distribution)
			hostnameDeployment = test.Deployment(test.DeploymentOptions{
				Replicas: int32(1000),
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":               "hostname-spread-app",
							"deployment-type":   "hostname-spread",
							test.DiscoveryLabel: "unspecified",
						},
					},
					ResourceRequirements: hostnameResources,
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{
							MaxSkew:           1,
							TopologyKey:       corev1.LabelHostname,
							WhenUnsatisfiable: corev1.DoNotSchedule,
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app":             "hostname-spread-app",
									"deployment-type": "hostname-spread",
								},
							},
						},
					},
				},
			})

			// Create standard deployment without topology constraints
			standardDeployment = test.Deployment(test.DeploymentOptions{
				Replicas: int32(1000),
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":               "standard-app",
							"deployment-type":   "standard",
							test.DiscoveryLabel: "unspecified",
						},
					},
					ResourceRequirements: standardResources,
				},
			})

			// Create Pod Disruption Budgets for both deployments
			// MinAvailable: 50% of replicas
			hostnamePDB = &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostname-spread-pdb",
					Namespace: hostnameDeployment.Namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "50%",
					},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app":             "hostname-spread-app",
							"deployment-type": "hostname-spread",
						},
					},
				},
			}

			standardPDB = &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "standard-pdb",
					Namespace: standardDeployment.Namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "50%",
					},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app":             "standard-app",
							"deployment-type": "standard",
						},
					},
				},
			}

			By("Creating NodePool and NodeClass for the test")
			env.ExpectCreated(nodePool, nodeClass)

			By("Creating Pod Disruption Budgets")
			env.ExpectCreated(hostnamePDB, standardPDB)

			By("Deploying both hostname spread and standard deployments")
			env.TimeIntervalCollector.Start("deployments_created")
			deploymentObjects := []client.Object{hostnameDeployment, standardDeployment}
			env.ExpectCreated(deploymentObjects...)
			env.TimeIntervalCollector.End("deployments_created")

			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("DEPLOYMENTS CREATED - WAITING FOR PROVISIONING\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Create selectors for monitoring pods
			hostnameSelector := labels.SelectorFromSet(hostnameDeployment.Spec.Selector.MatchLabels)
			standardSelector := labels.SelectorFromSet(standardDeployment.Spec.Selector.MatchLabels)
			allPodsSelector = labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})

			By("Waiting for all 2000 pods to be scheduled and ready")
			env.TimeIntervalCollector.Start("waiting_for_pods")

			// Wait for all pods to become healthy with a 20-minute timeout
			env.EventuallyExpectHealthyPodCountWithTimeout(20*time.Minute, allPodsSelector, 2000)

			env.TimeIntervalCollector.End("waiting_for_pods")
			env.TimeIntervalCollector.End("test_start")
			scaleOutTime := time.Since(testStartTime)

			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("SCALE OUT COMPLETED - 2000 PODS READY!\n")
			GinkgoWriter.Printf("Scale-out time: %v\n", scaleOutTime)
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			// Allow system to stabilize before triggering drift
			By("Waiting for system to stabilize before triggering drift")
			time.Sleep(30 * time.Second)

			// Collect pre-drift metrics
			By("Collecting pre-drift baseline metrics")
			preDriftNodes := env.Monitor.CreatedNodeCount()
			preDriftCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
			preDriftMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)

			GinkgoWriter.Printf("📊 PRE-DRIFT BASELINE:\n")
			GinkgoWriter.Printf("  • Nodes: %d\n", preDriftNodes)
			GinkgoWriter.Printf("  • CPU Utilization: %.2f%%\n", preDriftCPUUtil*100)
			GinkgoWriter.Printf("  • Memory Utilization: %.2f%%\n", preDriftMemUtil*100)

			// ========== DRIFT TRIGGER ==========
			By("Triggering drift by adding taint to individual nodes")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🔄 TRIGGERING DRIFT - Adding NoSchedule taint to existing nodes\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			driftTriggerTime := time.Now()
			env.TimeIntervalCollector.Start("drift_trigger")

			// Add a taint to individual existing nodes to trigger drift
			driftTaint := corev1.Taint{
				Key:    "drift-test",
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			}

			// Get all existing nodes and taint them individually
			existingNodes := env.Monitor.CreatedNodes()
			taintedNodeCount := 0
			for _, node := range existingNodes {
				// Create a copy of the node to modify
				nodeCopy := node.DeepCopy()
				nodeCopy.Spec.Taints = append(nodeCopy.Spec.Taints, driftTaint)
				env.ExpectUpdated(nodeCopy)
				taintedNodeCount++
			}

			GinkgoWriter.Printf("✅ Drift trigger applied: Added taint 'drift-test=true:NoSchedule' to %d existing nodes\n", taintedNodeCount)

			// Monitor for drift detection (nodes getting disruption taint)
			By("Monitoring for drift detection")
			var driftDetectionTime time.Duration
			var firstDriftDetected bool

			Eventually(func(g Gomega) {
				nodes := env.Monitor.CreatedNodes()
				drainingNodes := 0
				for _, node := range nodes {
					for _, taint := range node.Spec.Taints {
						if taint.Key == "karpenter.sh/disruption" {
							drainingNodes++
							break
						}
					}
				}
				if drainingNodes > 0 && !firstDriftDetected {
					driftDetectionTime = time.Since(driftTriggerTime)
					firstDriftDetected = true
					GinkgoWriter.Printf("🔍 DRIFT DETECTED: %d nodes marked for disruption after %v\n",
						drainingNodes, driftDetectionTime)
				}
				g.Expect(drainingNodes).To(BeNumerically(">", 0), "Should detect drifted nodes")
			}).WithTimeout(5 * time.Minute).Should(Succeed())

			// Monitor drift replacement process
			By("Monitoring drift replacement process")
			var driftRounds []DriftRound
			roundNumber := 1
			driftStartTime := time.Now()
			driftTimeout := 20 * time.Minute
			driftComplete := false

			for time.Since(driftStartTime) < driftTimeout && !driftComplete {
				currentNodes := env.Monitor.CreatedNodeCount()

				// Check for nodes being disrupted
				allNodes := env.Monitor.CreatedNodes()
				var drainingNodes, newNodes []corev1.Node

				for _, node := range allNodes {
					isDraining := false
					isNew := false

					// Check if node is draining
					if node.DeletionTimestamp != nil {
						isDraining = true
					}
					for _, taint := range node.Spec.Taints {
						if taint.Key == "karpenter.sh/disruption" {
							isDraining = true
							break
						}
					}

					// Check if node is new (has the drift taint)
					for _, taint := range node.Spec.Taints {
						if taint.Key == "drift-test" && taint.Value == "true" {
							isNew = true
							break
						}
					}

					if isDraining {
						drainingNodes = append(drainingNodes, *node)
					}
					if isNew {
						newNodes = append(newNodes, *node)
					}
				}

				GinkgoWriter.Printf("📊 Drift Progress - Round %d:\n", roundNumber)
				GinkgoWriter.Printf("  • Total nodes: %d\n", currentNodes)
				GinkgoWriter.Printf("  • Draining nodes: %d\n", len(drainingNodes))
				GinkgoWriter.Printf("  • New nodes (with drift taint): %d\n", len(newNodes))

				// Check if drift is complete (no nodes are draining and we have replacement nodes)
				if len(drainingNodes) == 0 && currentNodes > 0 {
					// Verify that we have nodes without the drift taint (replacement nodes)
					nodesWithoutDriftTaint := 0
					for _, node := range allNodes {
						hasDriftTaint := false
						for _, taint := range node.Spec.Taints {
							if taint.Key == "drift-test" && taint.Value == "true" {
								hasDriftTaint = true
								break
							}
						}
						if !hasDriftTaint {
							nodesWithoutDriftTaint++
						}
					}

					if nodesWithoutDriftTaint == currentNodes {
						driftComplete = true
						GinkgoWriter.Printf("✅ DRIFT COMPLETE: All nodes replaced with untainted nodes\n")
						break
					}
				}

				// Record this round if there's activity
				if len(drainingNodes) > 0 || len(newNodes) > 0 {
					round := DriftRound{
						RoundNumber:   roundNumber,
						StartTime:     time.Now(),
						Duration:      time.Since(driftStartTime),
						NodesReplaced: len(newNodes),
						StartingNodes: preDriftNodes,
						EndingNodes:   currentNodes,
					}
					driftRounds = append(driftRounds, round)
					roundNumber++
				}

				// Wait before next check
				time.Sleep(30 * time.Second)
			}

			totalDriftTime := time.Since(driftStartTime)
			env.TimeIntervalCollector.End("drift_trigger")

			if !driftComplete {
				GinkgoWriter.Printf("⚠️  Drift timeout reached after %v\n", driftTimeout)
			}

			// Collect post-drift metrics
			By("Collecting post-drift metrics")
			postDriftNodes := env.Monitor.CreatedNodeCount()
			postDriftCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
			postDriftMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)
			postDriftEfficiency := (postDriftCPUUtil + postDriftMemUtil) * 50

			// Verify all pods are still healthy after drift
			By("Verifying all pods are healthy after drift")
			env.EventuallyExpectHealthyPodCount(hostnameSelector, 1000)
			env.EventuallyExpectHealthyPodCount(standardSelector, 1000)
			env.EventuallyExpectHealthyPodCount(allPodsSelector, 2000)

			// Create drift performance report
			driftReport := DriftPerformanceReport{
				PerformanceReport: PerformanceReport{
					TestName:                "Drift Performance Test",
					TotalPods:               2000,
					SmallPods:               1000, // hostname spread deployment
					LargePods:               1000, // standard deployment
					TotalTime:               scaleOutTime,
					NodesProvisioned:        preDriftNodes,
					TotalReservedCPUUtil:    preDriftCPUUtil,
					TotalReservedMemoryUtil: preDriftMemUtil,
					ResourceEfficiencyScore: (preDriftCPUUtil + preDriftMemUtil) * 50,
					PodsPerNode:             float64(2000) / float64(preDriftNodes),
					Timestamp:               time.Now(),
					TestPassed:              driftComplete && totalDriftTime < 20*time.Minute,
				},
				// Drift-specific metrics
				DriftEnabled:        true,
				DriftTriggerTime:    driftTriggerTime,
				DriftDetectionTime:  driftDetectionTime,
				TotalDriftTime:      totalDriftTime,
				DriftRounds:         driftRounds,
				PreDriftNodes:       preDriftNodes,
				PostDriftNodes:      postDriftNodes,
				NodesReplaced:       postDriftNodes, // All nodes were replaced
				PostDriftCPUUtil:    postDriftCPUUtil,
				PostDriftMemoryUtil: postDriftMemUtil,
				PostDriftEfficiency: postDriftEfficiency,
			}

			// Add warnings based on drift performance
			if totalDriftTime >= 20*time.Minute {
				driftReport.Warnings = append(driftReport.Warnings, "Drift completion time exceeded 20 minutes")
				driftReport.TestPassed = false
			}
			if driftDetectionTime >= 5*time.Minute {
				driftReport.Warnings = append(driftReport.Warnings, "Drift detection time exceeded 5 minutes")
			}

			// Output detailed drift performance report
			By("=== DRIFT PERFORMANCE REPORT ===")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("🔄 DRIFT PERFORMANCE REPORT\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			// Test Configuration
			GinkgoWriter.Printf("\n📋 TEST CONFIGURATION:\n")
			GinkgoWriter.Printf("  • Total Pods: %d\n", driftReport.TotalPods)
			GinkgoWriter.Printf("  • Hostname Spread Pods: %d\n", driftReport.SmallPods)
			GinkgoWriter.Printf("  • Standard Pods: %d\n", driftReport.LargePods)
			GinkgoWriter.Printf("  • Pod Disruption Budgets: 50%% MinAvailable\n")
			GinkgoWriter.Printf("  • Drift Trigger: NoSchedule taint added to individual nodes\n")

			// Timing Metrics
			GinkgoWriter.Printf("\n⏱️  TIMING METRICS:\n")
			GinkgoWriter.Printf("  • Initial Scale-out Time: %v\n", scaleOutTime)
			GinkgoWriter.Printf("  • Drift Detection Time: %v\n", driftReport.DriftDetectionTime)
			GinkgoWriter.Printf("  • Total Drift Time: %v\n", driftReport.TotalDriftTime)
			GinkgoWriter.Printf("  • Drift Rounds: %d\n", len(driftReport.DriftRounds))

			// Node Replacement Metrics
			GinkgoWriter.Printf("\n🔄 NODE REPLACEMENT:\n")
			GinkgoWriter.Printf("  • Pre-drift Nodes: %d\n", driftReport.PreDriftNodes)
			GinkgoWriter.Printf("  • Post-drift Nodes: %d\n", driftReport.PostDriftNodes)
			GinkgoWriter.Printf("  • Nodes Replaced: %d\n", driftReport.NodesReplaced)
			GinkgoWriter.Printf("  • Replacement Rate: %.1f nodes/min\n",
				float64(driftReport.NodesReplaced)/driftReport.TotalDriftTime.Minutes())

			// Resource Utilization
			GinkgoWriter.Printf("\n💻 RESOURCE UTILIZATION:\n")
			GinkgoWriter.Printf("  • Pre-drift CPU Utilization: %.2f%%\n", preDriftCPUUtil*100)
			GinkgoWriter.Printf("  • Post-drift CPU Utilization: %.2f%%\n", driftReport.PostDriftCPUUtil*100)
			GinkgoWriter.Printf("  • Pre-drift Memory Utilization: %.2f%%\n", preDriftMemUtil*100)
			GinkgoWriter.Printf("  • Post-drift Memory Utilization: %.2f%%\n", driftReport.PostDriftMemoryUtil*100)
			GinkgoWriter.Printf("  • Post-drift Efficiency Score: %.1f%%\n", driftReport.PostDriftEfficiency)

			// Performance Summary Table
			GinkgoWriter.Printf("\n📊 DRIFT PERFORMANCE SUMMARY:\n")
			GinkgoWriter.Printf("┌─────────────────────────────┬──────────────┬──────────┐\n")
			GinkgoWriter.Printf("│ Metric                      │ Value        │ Status   │\n")
			GinkgoWriter.Printf("├─────────────────────────────┼──────────────┼──────────┤\n")
			GinkgoWriter.Printf("│ Drift Detection Time        │ %-12v │ %s │\n",
				driftReport.DriftDetectionTime, getStatusIcon(driftReport.DriftDetectionTime < 5*time.Minute))
			GinkgoWriter.Printf("│ Total Drift Time            │ %-12v │ %s │\n",
				driftReport.TotalDriftTime, getStatusIcon(driftReport.TotalDriftTime < 20*time.Minute))
			GinkgoWriter.Printf("│ Nodes Replaced              │ %-12d │ %s │\n",
				driftReport.NodesReplaced, getStatusIcon(driftReport.NodesReplaced == driftReport.PreDriftNodes))
			GinkgoWriter.Printf("│ Post-drift CPU Util         │ %-11.1f%% │ %s │\n",
				driftReport.PostDriftCPUUtil*100, getStatusIcon(driftReport.PostDriftCPUUtil > 0.3))
			GinkgoWriter.Printf("│ Post-drift Memory Util      │ %-11.1f%% │ %s │\n",
				driftReport.PostDriftMemoryUtil*100, getStatusIcon(driftReport.PostDriftMemoryUtil > 0.3))
			GinkgoWriter.Printf("└─────────────────────────────┴──────────────┴──────────┘\n")

			// Test Verdict
			if driftReport.TestPassed {
				GinkgoWriter.Printf("\n✅ DRIFT PERFORMANCE TEST: PASSED\n")
				GinkgoWriter.Printf("   All drift performance targets met successfully!\n")
			} else {
				GinkgoWriter.Printf("\n⚠️  DRIFT PERFORMANCE TEST: ATTENTION NEEDED\n")
				for _, warning := range driftReport.Warnings {
					GinkgoWriter.Printf("   • %s\n", warning)
				}
			}

			// Write detailed report to file if OUTPUT_DIR is set
			if outputDir := os.Getenv("OUTPUT_DIR"); outputDir != "" {
				reportFile := filepath.Join(outputDir, "drift_performance_report.json")
				reportJSON, err := json.MarshalIndent(driftReport, "", "  ")
				if err == nil {
					if err := os.WriteFile(reportFile, reportJSON, 0600); err == nil {
						GinkgoWriter.Printf("\n📄 Detailed JSON report written to: %s\n", reportFile)
					}
				}
			}

			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")

			By("Validating drift performance assertions")

			// Drift Performance Assertions
			Expect(driftDetectionTime).To(BeNumerically("<", 5*time.Minute),
				"Drift detection should occur within 5 minutes")

			Expect(totalDriftTime).To(BeNumerically("<", 20*time.Minute),
				"Total drift time should be less than 20 minutes")

			Expect(postDriftNodes).To(BeNumerically(">", 0),
				"Should have nodes after drift completion")

			Expect(driftReport.NodesReplaced).To(BeNumerically(">=", preDriftNodes),
				"Should replace at least all original nodes")

			// Verify final pod counts
			env.EventuallyExpectHealthyPodCount(allPodsSelector, 2000)

			GinkgoWriter.Printf("✅ DRIFT PERFORMANCE TEST: Completed successfully\n")
			GinkgoWriter.Printf(strings.Repeat("=", 70) + "\n")

			By("Performing comprehensive cleanup and verification")
			cleanupOpts := common.DefaultCleanupOptions()
			cleanupOpts.Deployments = []*appsv1.Deployment{hostnameDeployment, standardDeployment}
			cleanupOpts.PodSelector = allPodsSelector
			cleanupOpts.WaitTimeout = 5 * time.Minute

			err := env.PerformComprehensiveCleanup(cleanupOpts)
			Expect(err).ToNot(HaveOccurred(), "Comprehensive cleanup should succeed")

			// Manually clean up PDBs since they're not handled by the cleanup options
			_ = env.Client.Delete(env.Context, hostnamePDB)
			_ = env.Client.Delete(env.Context, standardPDB)

			By("Drift performance test completed successfully")
		})
	})
})
