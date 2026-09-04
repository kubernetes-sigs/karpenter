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
	methodLabel                  = "method"
	stageLabel                   = "stage"
	outcomeLabel                 = "outcome"
	stateLabel                   = "state"
	sourceLabel                  = "source"
	destinationLabel             = "destination"

	CandidateStagePossible = "possible"
	CandidateStageEligible = "eligible"

	SimulationStageEvaluate           = "evaluate"
	SimulationStageValidate           = "validate"
	SimulationPodSourceCandidate      = "candidate"
	SimulationPodSourcePending        = "pending"
	SimulationPodSourceDeleting       = "deleting"
	SimulationPodSourceTotal          = "total"
	SimulationDestinationExistingNode = "existing_node"
	SimulationDestinationNewNodeClaim = "new_nodeclaim"

	ValidationStageDelay            = "delay"
	ValidationStageCandidateRefresh = "candidate_refresh"
	ValidationStageSimulation       = "simulation"
	ValidationStageTotal            = "total"

	PassOutcomeNoCandidates = "no_candidates"
	PassOutcomeNoCommand    = "no_command"
	PassOutcomeSelected     = "selected"
	PassOutcomeError        = "error"

	SimulationOutcomeSchedulable       = "schedulable"
	SimulationOutcomeUnschedulable     = "unschedulable"
	SimulationOutcomeCandidateDeleting = "candidate_deleting"
	SimulationOutcomeTimeout           = "timeout"
	SimulationOutcomeError             = "error"

	TimeoutStateEligible    = "eligible"
	TimeoutStateEvaluated   = "evaluated"
	TimeoutStateUnevaluated = "unevaluated"
)

var candidateCountBuckets = []float64{0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000}
var podCountBuckets = []float64{0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000}

func init() {
	ConsolidationTimeoutsTotal.Add(0, map[string]string{ConsolidationTypeLabel: MultiNodeConsolidationType})
	ConsolidationTimeoutsTotal.Add(0, map[string]string{ConsolidationTypeLabel: SingleNodeConsolidationType})
}

var (
	PassesTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "passes_total",
			Help:      "Number of completed voluntary disruption method passes. Labeled by method, reason, consolidation type, and terminal outcome.",
		},
		[]string{methodLabel, metrics.ReasonLabel, ConsolidationTypeLabel, outcomeLabel},
	)
	LastEvaluatedTimestampSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "last_evaluated_timestamp_seconds",
			Help:      "Unix timestamp of the latest completed voluntary disruption method pass. Labeled by method, nodepool, reason, and consolidation type.",
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel},
	)
	Candidates = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "candidates",
			Help:      "Latest candidate count for a voluntary disruption method. Possible candidates passed common candidate construction; eligible candidates also passed the method-specific filter.",
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel, stageLabel},
	)
	OldestEligibleAgeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "oldest_eligible_age_seconds",
			Help:      "Age in seconds of the oldest candidate's durable disruption eligibility condition.",
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel},
	)
	CandidateEvaluationDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "candidate_evaluation_duration_seconds",
			Help:      "Duration of a complete disruption scheduling simulation in seconds. Labeled by method, nodepool scope, reason, consolidation type, and stage.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel, stageLabel},
	)
	SimulationsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "simulations_total",
			Help:      "Number of disruption scheduling simulations. Labeled by method, nodepool scope, reason, consolidation type, stage, and result.",
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel, stageLabel, outcomeLabel},
	)
	CandidateBatchSize = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "candidate_batch_size",
			Help:      "Number of candidates supplied to each disruption scheduling simulation.",
			Buckets:   candidateCountBuckets,
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel, stageLabel},
	)
	SimulationPodCount = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "simulation_pod_count",
			Help:      "Number of distinct pods supplied to a disruption scheduling simulation, labeled by pod source.",
			Buckets:   podCountBuckets,
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel, stageLabel, sourceLabel},
	)
	SimulationPodPlacementsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "simulation_pod_placements_total",
			Help:      "Number of candidate pods placed by selected disruption simulations, labeled by existing-node or new-nodeclaim destination.",
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel, destinationLabel},
	)
	ValidationDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "validation_duration_seconds",
			Help:      "Duration of voluntary disruption validation stages in seconds.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{methodLabel, metrics.ReasonLabel, ConsolidationTypeLabel, stageLabel},
	)
	CandidatesEvaluatedPerPass = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "candidates_evaluated_per_pass",
			Help:      "Number of distinct candidates included in evaluation-stage scheduling simulations during a completed method pass.",
			Buckets:   candidateCountBuckets,
		},
		[]string{methodLabel, metrics.ReasonLabel, ConsolidationTypeLabel, outcomeLabel},
	)
	TimeoutCandidateCount = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "timeout_candidate_count",
			Help:      "Candidate counts observed when a consolidation method times out. Labeled by eligible, distinctly evaluated, or unevaluated state.",
			Buckets:   candidateCountBuckets,
		},
		[]string{methodLabel, metrics.ReasonLabel, ConsolidationTypeLabel, stateLabel},
	)
	SelectedCandidatesTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "selected_candidates_total",
			Help:      "Number of candidates in validated commands accepted by the disruption queue.",
		},
		[]string{methodLabel, metrics.NodePoolLabel, metrics.ReasonLabel, ConsolidationTypeLabel, decisionLabel},
	)
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
	NodepoolDecisionsPerformed = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "decisions_by_nodepool_total",
			Help:      "Number of disruption decisions performed by nodepool. Labeled by nodepool name, disruption decision, reason, and consolidation type.",
		},
		[]string{metrics.NodePoolLabel, decisionLabel, metrics.ReasonLabel, ConsolidationTypeLabel},
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
	NodePoolNodesConsumingBudgets = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "nodes_consuming_budgets",
			Help:      "The number of nodes consuming the budget of a nodepool at a point in time. Labeled by NodePool.",
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
	ConsolidationScoreHistogram = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Name:      "consolidation_score",
			Help:      "Score of balanced consolidation moves. Labeled by decision, NodePool, and policy.",
			Buckets:   []float64{0.1, 0.25, 0.33, 0.5, 1.0, 2.0, 5.0, 10.0},
		},
		[]string{decisionLabel, metrics.NodePoolLabel, policyLabel},
	)
	ConsolidationMovesTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Name:      "consolidation_moves_total",
			Help:      "Number of balanced consolidation moves. Labeled by decision, NodePool, and policy.",
		},
		[]string{decisionLabel, metrics.NodePoolLabel, policyLabel},
	)
)
