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
