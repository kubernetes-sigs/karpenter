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
	terminateFirstReasonLabel    = "terminate_first_reason"
	methodLabel                  = "method"
	outcomeLabel                 = "outcome"
	stageLabel                   = "stage"
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
	// DecisionDim is the `decision` dimension for the voluntary-disruption decision
	// counters, whose value is the command's action.
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
				Name: string(TerminateFirstDecision),
				Help: "The candidate(s) were deleted without staging a replacement first; reactive provisioning refills afterward.",
			},
			{
				Name: string(RebootDecision),
				Help: "The candidate(s) were rebooted in place.",
			},
		},
	}
	// ApprovalDim is the `decision` dimension for the balanced-consolidation move
	// metrics, which score each candidate move and record whether it was approved or
	// rejected — a disjoint value set from DecisionDim, so it is a separate Label.
	ApprovalDim = opmetrics.Label{
		Name: decisionLabel,
		Help: "Whether a scored balanced-consolidation move was approved or rejected.",
		Values: []opmetrics.Value{
			{
				Name: string(ApprovedDecision),
				Help: "The move's cost savings justified the pod disruption; it was approved.",
			},
			{
				Name: string(RejectedDecision),
				Help: "The move's cost savings did not justify the pod disruption; it was rejected.",
			},
		},
	}
	Policy = opmetrics.Label{
		Name: policyLabel,
		Help: "The NodePool consolidation policy in effect for the move.",
	}
	TerminateFirstReasonDim = opmetrics.Label{
		Name: terminateFirstReasonLabel,
		Help: "Why the terminate-first command couldn't stage its replacement before terminating the node.",
		Values: []opmetrics.Value{
			{
				Name: string(TerminateFirstNoReservedCapacity),
				Help: "The replacement has no reserved capacity to launch into except the slot the node holds in a full capacity reservation.",
			},
			{
				Name: string(TerminateFirstStaticAtLimit),
				Help: "The node's static NodePool is at its node limit, so the replacement can't launch alongside it.",
			},
		},
	}
)

var (
	MethodRepair      = opmetrics.Value{Name: "repair", Help: "Repair of unhealthy nodes."}
	MethodStaticDrift = opmetrics.Value{Name: "static_drift", Help: "Replacement of drifted nodes in static NodePools."}
	MethodDrift       = opmetrics.Value{Name: "drift", Help: "Replacement of drifted nodes."}
	MethodUnknown     = opmetrics.Value{Name: "unknown", Help: "A disruption method without a well-known name."}
	MethodDim         = opmetrics.Label{
		Name:   methodLabel,
		Help:   "The disruption method that ran the evaluation.",
		Values: []opmetrics.Value{MethodRepair, EmptyConsolidationType, MethodStaticDrift, MethodDrift, MultiNodeConsolidationType, SingleNodeConsolidationType},
	}

	EvaluationOutcomeNoCandidates        = opmetrics.Value{Name: "no_candidates", Help: "No nodes were eligible for the method."}
	EvaluationOutcomeAlreadyConsolidated = opmetrics.Value{Name: "already_consolidated", Help: "Cluster state hasn't changed since the method last found nothing to do, so it was skipped."}
	EvaluationOutcomeBudgetBlocked       = opmetrics.Value{Name: "budget_blocked", Help: "No command was started and every candidate's NodePool had no disruptions allowed."}
	EvaluationOutcomeNoCommand           = opmetrics.Value{Name: "no_command", Help: "Candidates were evaluated but no valid command was found."}
	EvaluationOutcomeCommand             = opmetrics.Value{Name: "command", Help: "A command was found and started."}
	EvaluationOutcomeTimeout             = opmetrics.Value{Name: "timeout", Help: "The method timed out without starting a command."}
	EvaluationOutcomeValidationFailed    = opmetrics.Value{Name: "validation_failed", Help: "A command was found but failed validation, so it was not started."}
	EvaluationOutcomeError               = opmetrics.Value{Name: "error", Help: "The evaluation returned an error."}
	EvaluationOutcomeDim                 = opmetrics.Label{
		Name: outcomeLabel,
		Help: "The outcome of the disruption method evaluation.",
		Values: []opmetrics.Value{EvaluationOutcomeNoCandidates, EvaluationOutcomeAlreadyConsolidated, EvaluationOutcomeBudgetBlocked, EvaluationOutcomeNoCommand,
			EvaluationOutcomeCommand, EvaluationOutcomeTimeout, EvaluationOutcomeValidationFailed, EvaluationOutcomeError},
	}

	EvaluationStageEligible  = opmetrics.Value{Name: "eligible", Help: "Candidates passed to the method."}
	EvaluationStageEvaluated = opmetrics.Value{Name: "evaluated", Help: "Candidates included in a scheduling simulation during the evaluation. Only recorded by consolidation methods."}
	EvaluationStageSelected  = opmetrics.Value{Name: "selected", Help: "Candidates included in the commands the method started."}
	EvaluationStageDim       = opmetrics.Label{
		Name:   stageLabel,
		Help:   "How far through the method's candidate funnel the count was taken.",
		Values: []opmetrics.Value{EvaluationStageEligible, EvaluationStageEvaluated, EvaluationStageSelected},
	}

	BatchStageWindow    = opmetrics.Value{Name: "window", Help: "The largest batch the search could try."}
	BatchStageLastTried = opmetrics.Value{Name: "last_tried", Help: "The batch being simulated when the search stopped."}
	BatchStageDim       = opmetrics.Label{
		Name: stageLabel,
		Help: "Which batch size of the multi-node consolidation search was recorded.",
		Values: []opmetrics.Value{BatchStageWindow, BatchStageLastTried,
			{Name: EvaluationStageSelected.Name, Help: "The largest valid batch found, or 0 if none was."}},
	}
	SearchOutcomeCompleted = opmetrics.Value{Name: "completed", Help: "The search finished before the timeout."}
	SearchOutcomeDim       = opmetrics.Label{
		Name:   outcomeLabel,
		Help:   "Whether the multi-node consolidation search finished or hit its timeout.",
		Values: []opmetrics.Value{SearchOutcomeCompleted, {Name: EvaluationOutcomeTimeout.Name, Help: "The search hit its timeout."}},
	}
)

func init() {
	// Initialize the consolidation_type series that can time out to 0. Only the
	// multi- and single-node algorithms run a bounded search that can hit a timeout;
	// empty-node consolidation does not, so it is not pre-initialized here.
	for _, ct := range []opmetrics.Value{MultiNodeConsolidationType, SingleNodeConsolidationType} {
		ConsolidationTimeoutsTotal.Add(0, map[string]string{ConsolidationTypeLabel: ct.Name})
	}
	// Initialize every evaluation series to 0 so an absent series means "not exported" rather than "never happened".
	for _, m := range MethodDim.Values {
		for _, o := range EvaluationOutcomeDim.Values {
			EvaluationsTotal.Add(0, map[string]string{methodLabel: m.Name, outcomeLabel: o.Name})
		}
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
		opmetrics.Beta,
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
		opmetrics.GA,
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
		opmetrics.Alpha,
	)
	TerminateFirstDecisionsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "terminate_first_decisions_total",
			Help:      "Number of terminate-first disruption decisions performed. Labeled by nodepool name, reason, and why the replacement couldn't be staged first.",
		},
		[]opmetrics.Label{metrics.NodePool, metrics.DisruptionReason, TerminateFirstReasonDim},
		opmetrics.Alpha,
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
		opmetrics.Beta,
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
		opmetrics.Beta,
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
		opmetrics.Alpha,
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
		opmetrics.GA,
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
		opmetrics.Alpha,
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
		opmetrics.Beta,
	)
	ConsolidationScoreHistogram = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Name:      "consolidation_score",
			Help:      "Score of balanced consolidation moves. Labeled by decision, NodePool, and policy.",
			Buckets:   []float64{0.1, 0.25, 0.33, 0.5, 1.0, 2.0, 5.0, 10.0},
		},
		[]opmetrics.Label{ApprovalDim, metrics.NodePool, Policy},
		opmetrics.Alpha,
	)
	ConsolidationMovesTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Name:      "consolidation_moves_total",
			Help:      "Number of balanced consolidation moves. Labeled by decision, NodePool, and policy.",
		},
		[]opmetrics.Label{ApprovalDim, metrics.NodePool, Policy},
		opmetrics.Alpha,
	)
	EvaluationsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "evaluations_total",
			Help:      "Number of completed disruption method evaluations. Labeled by method and outcome.",
		},
		[]opmetrics.Label{MethodDim, EvaluationOutcomeDim},
		opmetrics.Alpha,
	)
	EvaluationCandidates = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "evaluation_candidates",
			Help:      "Number of candidates at each stage of a disruption method evaluation. Labeled by method and stage.",
			Buckets:   []float64{0, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
		},
		[]opmetrics.Label{MethodDim, EvaluationStageDim},
		opmetrics.Alpha,
	)
	MultiNodeBatchSize = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: voluntaryDisruptionSubsystem,
			Name:      "multi_node_batch_size",
			Help:      "Batch sizes of the multi-node consolidation search: the largest batch it could try, the batch being simulated when it stopped, and the largest valid batch found. Labeled by stage and whether the search timed out.",
			Buckets:   []float64{0, 1, 2, 5, 10, 25, 50, 75, 100, 101},
		},
		[]opmetrics.Label{BatchStageDim, SearchOutcomeDim},
		opmetrics.Alpha,
	)
	// NodeClaimsUnhealthyDisruptedTotal preserves the per-condition/per-image breakdown the retired node.health
	// controller emitted, which the reason-labeled karpenter_nodeclaims_disrupted_total loses. Labeled by the repair
	// condition, the owning NodePool, the capacity type, and the image ID.
	NodeClaimsUnhealthyDisruptedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeClaimSubsystem,
			Name:      "unhealthy_disrupted_total",
			Help:      "Number of unhealthy nodeclaims disrupted in total by node repair. Labeled by the condition the node was disrupted on, the owning nodepool, the capacity type, the image ID, and the termination mode.",
		},
		[]opmetrics.Label{RepairCondition, metrics.NodePool, metrics.CapacityType, ImageID, metrics.TerminationMode},
		opmetrics.Alpha,
	)
)

const (
	conditionLabel = "condition"
	imageIDLabel   = "image_id"
)

var (
	RepairCondition = opmetrics.Label{
		Name: conditionLabel,
		Help: "The node status condition type that triggered node repair disruption.",
	}
	ImageID = opmetrics.Label{
		Name: imageIDLabel,
		Help: "The image ID of the node that was disrupted.",
	}
)
