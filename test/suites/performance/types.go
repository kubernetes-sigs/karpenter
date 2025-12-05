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
	TestType                string        `json:"test_type"`
	TotalPods               int           `json:"total_pods"`
	TotalNodes              int           `json:"total_nodes"`
	TotalTime               time.Duration `json:"total_time"`
	PodsNetChange           int           `json:"change_in_pod_count"`
	NodesNetChange          int           `json:"change_in_node_count"`
	TotalReservedCPUUtil    float64       `json:"total_reserved_cpu_utilization"`
	TotalReservedMemoryUtil float64       `json:"total_reserved_memory_utilization"`
	ResourceEfficiencyScore float64       `json:"resource_efficiency_score"`
	PodsPerNode             float64       `json:"pods_per_node"`
	Rounds                  int           `json:"rounds"`
	Timestamp               time.Time     `json:"timestamp"`
}
