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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	. "github.com/onsi/ginkgo/v2"

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// OutputPerformanceReport outputs a performance report to console and file
func OutputPerformanceReport(report *PerformanceReport, filePrefix string) {
	// Console output (fallback)
	GinkgoWriter.Printf("\n=== %s PERFORMANCE REPORT ===\n", report.TestType)
	GinkgoWriter.Printf("Test: %s\n", report.TestName)
	GinkgoWriter.Printf("Type: %s\n", report.TestType)
	GinkgoWriter.Printf("Total Time: %v\n", report.TotalTime)
	GinkgoWriter.Printf("Total Pods: %d (Net Change: %+d)\n", report.TotalPods, report.PodsNetChange)
	GinkgoWriter.Printf("Total Nodes: %d (Net Change: %+d)\n", report.TotalNodes, report.NodesNetChange)
	GinkgoWriter.Printf("CPU Utilization: %.2f%%\n", report.TotalReservedCPUUtil*100)
	GinkgoWriter.Printf("Memory Utilization: %.2f%%\n", report.TotalReservedMemoryUtil*100)
	GinkgoWriter.Printf("Efficiency Score: %.1f%%\n", report.ResourceEfficiencyScore)
	GinkgoWriter.Printf("Pods per Node: %.1f\n", report.PodsPerNode)
	GinkgoWriter.Printf("Rounds: %d\n", report.Rounds)

	// File output
	if outputDir := os.Getenv("OUTPUT_DIR"); outputDir != "" {
		reportFile := filepath.Join(outputDir, fmt.Sprintf("%s_performance_report.json", filePrefix))
		reportJSON, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			if err := os.WriteFile(reportFile, reportJSON, 0600); err == nil {
				GinkgoWriter.Printf("Report written to: %s\n", reportFile)
			}
		}
	}
}

// ReportScaleOut monitors a scale-out operation and returns a performance report.
// This function waits for the specified number of pods to become healthy and measures
// the time taken, resource utilization, and node efficiency.
//
// Parameters:
//   - env: The test environment
//   - testName: Name of the test for reporting
//   - expectedPods: Expected number of healthy pods
//   - timeout: Maximum time to wait for scale-out completion
//
// Returns a PerformanceReport with scale-out metrics and timing information.
func ReportScaleOut(env *common.Environment, testName string, expectedPods int, timeout time.Duration) (*PerformanceReport, error) {
	startTime := time.Now()

	// Wait for all pods to be healthy
	allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	if expectedPods > 0 {
		env.EventuallyExpectHealthyPodCountWithTimeout(timeout, allPodsSelector, expectedPods)
	}

	totalTime := time.Since(startTime)

	// Collect metrics
	nodeCount := env.Monitor.CreatedNodeCount()
	avgCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
	avgMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)

	// Calculate derived metrics
	resourceEfficiencyScore := (avgCPUUtil*90 + avgMemUtil*10)
	podsPerNode := float64(0)
	if nodeCount > 0 {
		podsPerNode = float64(expectedPods) / float64(nodeCount)
	}

	return &PerformanceReport{
		TestName:                testName,
		TestType:                "scale-out",
		TotalPods:               expectedPods,
		TotalNodes:              nodeCount,
		TotalTime:               totalTime,
		PodsNetChange:           expectedPods,
		NodesNetChange:          nodeCount,
		TotalReservedCPUUtil:    avgCPUUtil,
		TotalReservedMemoryUtil: avgMemUtil,
		ResourceEfficiencyScore: resourceEfficiencyScore,
		PodsPerNode:             podsPerNode,
		Rounds:                  1, // Scale-out is always 1 round
		Timestamp:               time.Now(),
	}, nil
}

// ReportConsolidation monitors a consolidation operation and returns a performance report.
// This function waits for pods to scale down and then monitors node consolidation rounds.
//
// Parameters:
//   - env: The test environment
//   - testName: Name of the test for reporting
//   - initialPods: Initial number of pods before consolidation
//   - finalPods: Expected final number of pods after consolidation
//   - initialNodes: Initial number of nodes before consolidation
//   - timeout: Maximum time to wait for consolidation completion
//
// Returns a PerformanceReport with consolidation metrics and timing information.
func ReportConsolidation(env *common.Environment, testName string, initialPods, finalPods, initialNodes int, timeout time.Duration) (*PerformanceReport, error) {
	startTime := time.Now()

	// Wait for pods to scale down first
	allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	if finalPods > 0 {
		env.EventuallyExpectHealthyPodCountWithTimeout(timeout, allPodsSelector, finalPods)
	}

	// Monitor consolidation rounds
	consolidationRounds, _ := monitorConsolidationRounds(env, timeout)

	totalTime := time.Since(startTime)

	// Collect final metrics
	finalNodes := env.Monitor.CreatedNodeCount()
	avgCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
	avgMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)

	// Calculate derived metrics
	resourceEfficiencyScore := (avgCPUUtil*90 + avgMemUtil*10)
	podsPerNode := float64(0)
	if finalNodes > 0 {
		podsPerNode = float64(finalPods) / float64(finalNodes)
	}

	return &PerformanceReport{
		TestName:                testName,
		TestType:                "consolidation",
		TotalPods:               finalPods,
		TotalNodes:              finalNodes,
		TotalTime:               totalTime,
		PodsNetChange:           finalPods - initialPods,
		NodesNetChange:          finalNodes - initialNodes,
		TotalReservedCPUUtil:    avgCPUUtil,
		TotalReservedMemoryUtil: avgMemUtil,
		ResourceEfficiencyScore: resourceEfficiencyScore,
		PodsPerNode:             podsPerNode,
		Rounds:                  len(consolidationRounds),
		Timestamp:               time.Now(),
	}, nil
}

// ReportDrift monitors a drift operation and returns a performance report.
// This function monitors node replacement during drift operations and measures
// the time taken and number of replacement rounds.
//
// Parameters:
//   - env: The test environment
//   - testName: Name of the test for reporting
//   - expectedPods: Expected number of pods (should remain constant during drift)
//   - timeout: Maximum time to wait for drift completion
//
// Returns a PerformanceReport with drift metrics and timing information.
func ReportDrift(env *common.Environment, testName string, expectedPods int, timeout time.Duration) (*PerformanceReport, error) {
	startTime := time.Now()
	initialNodeCount := env.Monitor.CreatedNodeCount()

	// Track node replacement during drift
	driftRounds := 0
	lastReplacementTime := time.Now()
	driftStartTime := time.Now()

	// Monitor for node replacements during drift
	for time.Since(driftStartTime) < timeout {
		// Check if nodes are being replaced (draining/terminating)
		var drainingNodes []corev1.Node
		allNodes := env.Monitor.CreatedNodes()
		for _, node := range allNodes {
			// Check if node has draining taint or is being deleted
			if node.DeletionTimestamp != nil {
				drainingNodes = append(drainingNodes, *node)
			}
			for _, taint := range node.Spec.Taints {
				if taint.Key == "karpenter.sh/disrupted" {
					drainingNodes = append(drainingNodes, *node)
					break
				}
			}
		}

		// If we detect draining nodes, this indicates a drift replacement round
		if len(drainingNodes) > 0 {
			lastReplacementTime = time.Now()
			driftRounds++

			// Wait for replacement to complete
			time.Sleep(30 * time.Second)
		}

		// Check for stability (no replacements for 2 minutes)
		if time.Since(lastReplacementTime) >= 2*time.Minute {
			break
		}

		// Wait before next check
		time.Sleep(15 * time.Second)
	}

	// Ensure all pods are healthy after drift
	allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	if expectedPods > 0 {
		env.EventuallyExpectHealthyPodCountWithTimeout(timeout/2, allPodsSelector, expectedPods)
	}

	totalTime := time.Since(startTime)
	finalNodeCount := env.Monitor.CreatedNodeCount()

	// Collect metrics
	avgCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
	avgMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)

	// Calculate derived metrics
	resourceEfficiencyScore := (avgCPUUtil*90 + avgMemUtil*10)
	podsPerNode := float64(0)
	if finalNodeCount > 0 {
		podsPerNode = float64(expectedPods) / float64(finalNodeCount)
	}

	// If no drift rounds were detected, assume at least 1 round occurred
	if driftRounds == 0 {
		driftRounds = 1
	}

	return &PerformanceReport{
		TestName:                testName,
		TestType:                "drift",
		TotalPods:               expectedPods,
		TotalNodes:              finalNodeCount,
		TotalTime:               totalTime,
		PodsNetChange:           0,                                 // Pods don't change in drift
		NodesNetChange:          finalNodeCount - initialNodeCount, // Net change in nodes (should be ~0 for drift)
		TotalReservedCPUUtil:    avgCPUUtil,
		TotalReservedMemoryUtil: avgMemUtil,
		ResourceEfficiencyScore: resourceEfficiencyScore,
		PodsPerNode:             podsPerNode,
		Rounds:                  driftRounds,
		Timestamp:               time.Now(),
	}, nil
}

// monitorConsolidationRounds monitors node consolidation and returns consolidation rounds.
// This is a helper function used by ReportConsolidation to track individual consolidation rounds.
func monitorConsolidationRounds(env *common.Environment, timeout time.Duration) ([]ConsolidationRound, time.Duration) {
	var consolidationRounds []ConsolidationRound
	roundNumber := 1
	lastDrainingTime := time.Now()
	consolidationStartTime := time.Now()

	for time.Since(consolidationStartTime) < timeout {
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
				if taint.Key == "karpenter.sh/disrupted" {
					drainingNodes = append(drainingNodes, *node)
					break
				}
			}
		}

		// If we detect draining nodes, record this as a consolidation round
		if len(drainingNodes) > 0 {
			lastDrainingTime = time.Now()

			// Wait for this round to complete
			roundStartTime := time.Now()
			time.Sleep(25 * time.Second)
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
			roundNumber++
		}

		// Check for stability (no draining for 3 minutes)
		if time.Since(lastDrainingTime) >= 3*time.Minute {
			break
		}

		// Wait before next check
		time.Sleep(30 * time.Second)
	}

	totalConsolidationTime := time.Since(consolidationStartTime)

	return consolidationRounds, totalConsolidationTime
}

// Convenience functions for common monitoring patterns

// ReportScaleOutWithOutput monitors scale-out and automatically outputs the report
func ReportScaleOutWithOutput(env *common.Environment, testName string, expectedPods int, timeout time.Duration, filePrefix string) (*PerformanceReport, error) {
	report, err := ReportScaleOut(env, testName, expectedPods, timeout)
	if err != nil {
		return nil, err
	}

	OutputPerformanceReport(report, filePrefix)
	return report, nil
}

// ReportConsolidationWithOutput monitors consolidation and automatically outputs the report
func ReportConsolidationWithOutput(env *common.Environment, testName string, initialPods, finalPods, initialNodes int, timeout time.Duration, filePrefix string) (*PerformanceReport, error) {
	report, err := ReportConsolidation(env, testName, initialPods, finalPods, initialNodes, timeout)
	if err != nil {
		return nil, err
	}

	OutputPerformanceReport(report, filePrefix)
	return report, nil
}

// ReportDriftWithOutput monitors drift and automatically outputs the report
func ReportDriftWithOutput(env *common.Environment, testName string, expectedPods int, timeout time.Duration, filePrefix string) (*PerformanceReport, error) {
	report, err := ReportDrift(env, testName, expectedPods, timeout)
	if err != nil {
		return nil, err
	}

	OutputPerformanceReport(report, filePrefix)
	return report, nil
}
