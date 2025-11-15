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

package disruption

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	voluntaryDisruptionSubsystem = "voluntary_disruption"
	decisionLabel                = "decision"
	ConsolidationTypeLabel       = "consolidation_type"
	CandidatesIneligible         = "candidates_ineligible"

	// Label constants
	ClusterCostLabel              = "cluster_cost"
	TotalCPURequestsLabel         = "total_cpu_requests"
	TotalMemoryRequestsLabel      = "total_memory_requests"
	TotalNodeCountLabel           = "total_node_count"
	TotalDesiredPodCountLabel     = "total_desired_pod_count"
	InvolvesPodAntiAffinityLabel  = "involves_pod_anti_affinity"
	NodePoolNameLabel             = "nodepool_name"
	PodCPURequestChangeRatioLabel = "pod_cpu_request_change_ratio"
	PodMemRequestChangeRatioLabel = "pod_mem_request_change_ratio"
)

func init() {
	ConsolidationTimeoutsTotal.Add(0, map[string]string{ConsolidationTypeLabel: MultiNodeConsolidationType})
	ConsolidationTimeoutsTotal.Add(0, map[string]string{ConsolidationTypeLabel: SingleNodeConsolidationType})
}

var (
	EvaluationDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "decision_evaluation_duration_seconds",
			Help:      "Duration of the disruption decision evaluation process in seconds. Labeled by method and consolidation type.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{metrics.ReasonLabel, ConsolidationTypeLabel},
	)
	DecisionsPerformedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "decisions_total",
			Help:      "Number of disruption decisions performed. Labeled by disruption decision, reason, and consolidation type.",
		},
		[]string{decisionLabel, metrics.ReasonLabel, ConsolidationTypeLabel},
	)
	EligibleNodes = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "eligible_nodes",
			Help:      "Number of nodes eligible for disruption by Karpenter. Labeled by disruption reason.",
		},
		[]string{metrics.ReasonLabel},
	)
	ConsolidationTimeoutsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "consolidation_timeouts_total",
			Help:      "Number of times the Consolidation algorithm has reached a timeout. Labeled by consolidation type.",
		},
		[]string{ConsolidationTypeLabel},
	)
	FailedValidationsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "failed_validations_total",
			Help:      "Number of candidates that were selected for disruption but failed validation. Labeled by consolidation type.",
		},
		[]string{ConsolidationTypeLabel},
	)
	NodePoolAllowedDisruptions = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "allowed_disruptions",
			Help:      "The number of nodes for a given NodePool that can be concurrently disrupting at a point in time. Labeled by NodePool. Note that allowed disruptions can change very rapidly, as new nodes may be created and others may be deleted at any point.",
		},
		[]string{metrics.NodePoolLabel, metrics.ReasonLabel},
	)
	DisruptionQueueFailuresTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "queue_failures_total",
			Help:      "The number of times that an enqueued disruption decision failed. Labeled by disruption method.",
		},
		[]string{decisionLabel, metrics.ReasonLabel, ConsolidationTypeLabel},
	)

	DecisionDimensions = []string{
		ClusterCostLabel,
		TotalCPURequestsLabel,
		TotalMemoryRequestsLabel,
		TotalNodeCountLabel,
		TotalDesiredPodCountLabel,
		ConsolidationTypeLabel,
		NodePoolNameLabel,
		PodCPURequestChangeRatioLabel,
		PodMemRequestChangeRatioLabel,
	}
	// Change ratios are calculated with the following formula:
	//   Value at Beginning / Value at End

	// ClusterCostChangeRatio tracks the ratio of cluster cost before and after a decision
	ClusterCostChangeRatio = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "cluster_cost_change_ratio",
			Help:      "ALPHA METRIC. Ratio of cluster cost at decision start to cluster cost at decision end. Labeled by decision dimensions.",
		},
		DecisionDimensions,
	)

	// CPURequestsChangeRatio tracks the ratio of total desired pod CPU requests before and after a decision
	CPURequestsChangeRatio = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "cpu_requests_change_ratio",
			Help:      "ALPHA METRIC. Ratio of total desired pod CPU requests at decision start to total desired pod CPU requests at decision end. Labeled by decision dimensions.",
		},
		DecisionDimensions,
	)

	// MemoryRequestsChangeRatio tracks the ratio of total desired pod memory requests before and after a decision
	MemoryRequestsChangeRatio = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "memory_requests_change_ratio",
			Help:      "ALPHA METRIC. Ratio of total desired pod memory requests at decision start to total desired pod memory requests at decision end. Labeled by decision dimensions.",
		},
		DecisionDimensions,
	)

	// NodeCountChangeRatio tracks the ratio of total node count before and after a decision
	NodeCountChangeRatio = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "node_count_change_ratio",
			Help:      "ALPHA METRIC. Ratio of total node count at decision start to total node count at decision end. Labeled by decision dimensions.",
		},
		DecisionDimensions,
	)

	// DesiredPodCountChangeRatio tracks the ratio of total desired pod count before and after a decision
	DesiredPodCountChangeRatio = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "desired_pod_count_change_ratio",
			Help:      "ALPHA METRIC. Ratio of total desired pod count at decision start to total desired pod count at decision end. Labeled by decision dimensions.",
		},
		DecisionDimensions,
	)
	// DecisionTrackerErrors tracks the number of internal errors in the decision tracker
	DecisionTrackerErrors = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "internal_errors_total",
			Help:      "ALPHA METRIC. Total Errors during the course of decision tracking",
		},
		[]string{},
	)
	// DecisionTrackerCacheExpirations tracks the number of cache expirations in the decision tracker
	DecisionTrackerCacheExpirations = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "cache_expirations_total",
			Help:      "ALPHA METRIC. Total Cache entry expirations during the course of decision tracking",
		},
		[]string{},
	)
)
