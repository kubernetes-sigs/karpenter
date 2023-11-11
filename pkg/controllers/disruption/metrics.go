/*
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
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/aws/karpenter-core/pkg/metrics"
)

func init() {
	crmetrics.Registry.MustRegister(disruptionEvaluationDurationHistogram, disruptionReplacementNodeClaimInitializedHistogram, disruptionReplacementNodeClaimFailedCounter,
		disruptionActionsPerformedCounter, disruptionEligibleNodesGauge, disruptionConsolidationTimeoutTotalCounter)
}

const (
	disruptionSubsystem    = "disruption"
	actionLabel            = "action"
	methodLabel            = "method"
	consolidationTypeLabel = "consolidation_type"
)

var (
	disruptionEvaluationDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: disruptionSubsystem,
			Name:      "evaluation_duration_seconds",
			Help:      "Duration of the disruption evaluation process in seconds.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{methodLabel, consolidationTypeLabel},
	)
	disruptionReplacementNodeClaimInitializedHistogram = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: disruptionSubsystem,
			Name:      "replacement_nodeclaim_initialized_seconds",
			Help:      "Amount of time required for a replacement nodeclaim to become initialized.",
			Buckets:   metrics.DurationBuckets(),
		},
	)
	disruptionReplacementNodeClaimFailedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: disruptionSubsystem,
			Name:      "replacement_nodeclaim_failures_total",
			Help:      "The number of times that Karpenter failed to launch a replacement node for disruption. Labeled by disruption method.",
		},
		[]string{methodLabel, consolidationTypeLabel},
	)
	disruptionActionsPerformedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: disruptionSubsystem,
			Name:      "actions_performed_total",
			Help:      "Number of disruption actions performed. Labeled by disruption method.",
		},
		[]string{actionLabel, methodLabel, consolidationTypeLabel},
	)
	disruptionEligibleNodesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: disruptionSubsystem,
			Name:      "eligible_nodes",
			Help:      "Number of nodes eligible for disruption by Karpenter. Labeled by disruption method.",
		},
		[]string{methodLabel, consolidationTypeLabel},
	)
	disruptionConsolidationTimeoutTotalCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: disruptionSubsystem,
			Name:      "consolidation_timeouts_total",
			Help:      "Number of times the Consolidation algorithm has reached a timeout. Labeled by consolidation type.",
		},
		[]string{consolidationTypeLabel},
	)
)
