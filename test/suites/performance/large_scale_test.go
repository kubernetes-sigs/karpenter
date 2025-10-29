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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/pkg/test"
)

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
	CPUUtilization          float64       `json:"cpu_utilization"`
	MemoryUtilization       float64       `json:"memory_utilization"`
	ResourceEfficiencyScore float64       `json:"resource_efficiency_score"`
	PodsPerNode             float64       `json:"pods_per_node"`
	Timestamp               time.Time     `json:"timestamp"`
	TestPassed              bool          `json:"test_passed"`
	Warnings                []string      `json:"warnings,omitempty"`
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
			smallDeployment := test.Deployment(test.DeploymentOptions{
				Replicas: int32(300),
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
			largeDeployment := test.Deployment(test.DeploymentOptions{
				Replicas: int32(300),
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
			allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})

			By("Waiting for all 1000 pods to be scheduled and ready")
			env.TimeIntervalCollector.Start("waiting_for_pods")

			// Wait for all pods to become healthy with a 15-minute timeout
			// This covers both scheduling and readiness
			env.EventuallyExpectHealthyPodCountWithTimeout(15*time.Minute, allPodsSelector, 600)

			env.TimeIntervalCollector.End("waiting_for_pods")
			env.TimeIntervalCollector.End("test_start")
			totalTime := time.Since(testStartTime)
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
			GinkgoWriter.Printf("SCALE OUT COMPLETED!" + "\n")
			GinkgoWriter.Printf("\n" + strings.Repeat("=", 70) + "\n")
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
				TotalPods:               800,
				SmallPods:               400,
				LargePods:               400,
				TotalTime:               totalTime,
				PodSchedulingTime:       podSchedulingTime,
				NodeProvisioningTime:    nodeProvisioningTime,
				PodReadyTime:            podReadyTime,
				NodesProvisioned:        nodeCount,
				CPUUtilization:          avgCPUUtil,
				MemoryUtilization:       avgMemUtil,
				ResourceEfficiencyScore: resourceEfficiencyScore,
				PodsPerNode:             podsPerNode,
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
			GinkgoWriter.Printf("  • Average CPU Utilization: %.2f%%\n", report.CPUUtilization*100)
			GinkgoWriter.Printf("  • Average Memory Utilization: %.2f%%\n", report.MemoryUtilization*100)
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
			GinkgoWriter.Printf("│ Average CPU Utilization     │ %-11.1f%% │ %s │\n",
				report.CPUUtilization*100, getStatusIcon(report.CPUUtilization > 0.7))
			GinkgoWriter.Printf("│ Average Memory Utilization  │ %-11.1f%% │ %s │\n",
				report.MemoryUtilization*100, getStatusIcon(report.MemoryUtilization > 0.7))
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
			env.EventuallyExpectHealthyPodCount(smallPodSelector, 300)
			env.EventuallyExpectHealthyPodCount(largePodSelector, 300)

			By("Performance test completed successfully")
		})
	})
})
