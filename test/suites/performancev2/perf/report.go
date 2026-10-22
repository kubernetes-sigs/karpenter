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

package perf

import "time"

type Result struct {
	Provider string         `json:"provider"`
	Phase    string         `json:"phase"`
	Commit   string         `json:"commit"`
	Sizes    map[Vector]int `json:"sizes"`
	Kind     string         `json:"kind"`
	Time     time.Time      `json:"timestamp"`
	Total    time.Duration  `json:"total_time"`
	Active   time.Duration  `json:"active_time"`

	Pods           int    `json:"pods"`
	PodsNetChange  int    `json:"pods_net_change"`
	Nodes          int    `json:"nodes"`
	NodesNetChange int    `json:"nodes_net_change"`
	PodsDisrupted  uint64 `json:"pods_disrupted"`
	Rounds         int    `json:"rounds"`

	ReservedCPUUtil    float64 `json:"reserved_cpu_utilization"`
	ReservedMemoryUtil float64 `json:"reserved_memory_utilization"`

	Resources        ResourceStats `json:"resources"`
	StartupSeconds   float64       `json:"startup_seconds,omitempty"`
	StartupCPUSecond float64       `json:"startup_cpu_seconds,omitempty"`
	PodReady         Quantiles     `json:"pod_ready_seconds"`

	LatencyStats map[string]HistogramStats `json:"latency_stats,omitempty"`
	Counters     map[string]uint64         `json:"counters,omitempty"`
	Gates        []GateResult              `json:"gates"`
}

// Quantiles summarizes per-pod seconds from creation to Ready, over pods created during a phase.
type Quantiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}
