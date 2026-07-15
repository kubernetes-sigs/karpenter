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
	policyLabel                  = "policy"
)

var (
	MultiNodeConsolidationType = opmetrics.Value{
		Name: "multi",
		Help: "Consolidation that considers removing multiple nodes at once.",
	}
	SingleNodeConsolidationType = opmetrics.Value{
		Name: "single",
		Help: "Consolidation that considers removing a single node.",
	}
	EmptyConsolidationType = opmetrics.Value{
		Name: "empty",
		Help: "Consolidation that removes empty nodes.",
	}
)

var (
	ConsolidationType = opmetrics.Label{
		Name:   ConsolidationTypeLabel,
		Help:   "The consolidation algorithm that produced the decision.",
		Values: []opmetrics.Value{MultiNodeConsolidationType, SingleNodeConsolidationType, EmptyConsolidationType},
	}
	DecisionDim = opmetrics.Label{
		Name: decisionLabel,
		Help: "The disruption decision taken for the candidate(s).",
		Values: []opmetrics.Value{
			{
				Name: string(NoOpDecision),
				Help: "No disruption action was taken.",
			},
			{
				Name: string(ReplaceDecision),
				Help: "The candidate(s) were replaced with more efficient capacity.",
			},
			{
				Name: string(DeleteDecision),
				Help: "The candidate(s) were deleted without replacement.",
			},
			{
				Name: string(ApprovedDecision),
				Help: "The disruption decision was approved for execution.",
			},
			{
				Name: string(RejectedDecision),
				Help: "The disruption decision was rejected before execution.",
			},
		},
	}
	Policy = opmetrics.Label{
		Name: policyLabel,
		Help: "The NodePool consolidation policy in effect for the move.",
	}
)

func init() {
	// Initialize the consolidation_type series that can time out to 0. Only the
	// multi- and single-node algorithms run a bounded search that can hit a timeout;
	// empty-node consolidation does not, so it is not pre-initialized here.
	for _, ct := range []opmetrics.Value{MultiNodeConsolidationType, SingleNodeConsolidationType} {
		ConsolidationTimeoutsTotal.Add(0, map[string]string{ConsolidationTypeLabel: ct.Name})
	}
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
		[]opmetrics.Label{metrics.DisruptionReason, ConsolidationType},
	)
	DecisionsPerformedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "decisions_total",
			Help:      "Number of disruption decisions performed. Labeled by disruption decision, reason, and consolidation type.",
		},
		[]opmetrics.Label{DecisionDim, metrics.DisruptionReason, ConsolidationType},
	)
	NodepoolDecisionsPerformed = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "decisions_by_nodepool_total",
			Help:      "Number of disruption decisions performed by nodepool. Labeled by nodepool name, disruption decision, reason, and consolidation type.",
		},
		[]opmetrics.Label{metrics.NodePool, DecisionDim, metrics.DisruptionReason, ConsolidationType},
	)
	EligibleNodes = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "eligible_nodes",
			Help:      "Number of nodes eligible for disruption by Karpenter. Labeled by disruption reason.",
		},
		[]opmetrics.Label{metrics.DisruptionReason},
	)
	ConsolidationTimeoutsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "consolidation_timeouts_total",
			Help:      "Number of times the Consolidation algorithm has reached a timeout. Labeled by consolidation type.",
		},
		[]opmetrics.Label{ConsolidationType},
	)
	FailedValidationsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "failed_validations_total",
			Help:      "Number of candidates that were selected for disruption but failed validation. Labeled by consolidation type.",
		},
		[]opmetrics.Label{ConsolidationType},
	)
	NodePoolAllowedDisruptions = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "allowed_disruptions",
			Help:      "The number of nodes for a given NodePool that can be concurrently disrupting at a point in time. Labeled by NodePool. Note that allowed disruptions can change very rapidly, as new nodes may be created and others may be deleted at any point.",
		},
		[]opmetrics.Label{metrics.NodePool, metrics.DisruptionReason},
	)
	NodePoolNodesConsumingBudgets = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "nodes_consuming_budgets",
			Help:      "The number of nodes consuming the budget of a nodepool at a point in time. Labeled by NodePool.",
		},
		[]opmetrics.Label{metrics.NodePool, metrics.DisruptionReason},
	)
	DisruptionQueueFailuresTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "queue_failures_total",
			Help:      "The number of times that an enqueued disruption decision failed. Labeled by disruption method.",
		},
		[]opmetrics.Label{DecisionDim, metrics.DisruptionReason, ConsolidationType},
	)
	ConsolidationScoreHistogram = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Name:      "consolidation_score",
			Help:      "Score of balanced consolidation moves. Labeled by decision, NodePool, and policy.",
			Buckets:   []float64{0.1, 0.25, 0.33, 0.5, 1.0, 2.0, 5.0, 10.0},
		},
		[]opmetrics.Label{DecisionDim, metrics.NodePool, Policy},
	)
	ConsolidationMovesTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Name:      "consolidation_moves_total",
			Help:      "Number of balanced consolidation moves. Labeled by decision, NodePool, and policy.",
		},
		[]opmetrics.Label{DecisionDim, metrics.NodePool, Policy},
	)
	DriftBackoffsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "drift_backoffs_total",
			Help:      "The number of times a NodePool entered or escalated drift replacement back-off after an unrecoverable failure. Labeled by NodePool.",
		},
		[]string{metrics.NodePoolLabel},
	)
)
