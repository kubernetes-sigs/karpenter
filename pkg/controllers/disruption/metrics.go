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
	crmetrics.Registry.MustRegister(deprovisioningDurationHistogram, deprovisioningReplacementNodeInitializedHistogram, deprovisioningActionsPerformedCounter,
		deprovisioningEligibleMachinesGauge, deprovisioningReplacementNodeLaunchFailedCounter, deprovisioningConsolidationTimeoutsCounter,
		disruptionEvaluationDurationHistogram, disruptionReplacementNodeClaimInitializedHistogram, disruptionReplacementNodeClaimFailedCounter,
		disruptionActionsPerformedCounter, disruptionEligibleNodesGauge, disruptionConsolidationTimeoutTotalCounter)
}

const (
	deprovisioningSubsystem = "deprovisioning"
	deprovisionerLabel      = "deprovisioner"

	disruptionSubsystem    = "disruption"
	actionLabel            = "action"
	methodLabel            = "method"
	consolidationTypeLabel = "consolidation_type"

	multiMachineConsolidationLabelValue  = "multi-machine"
	singleMachineConsolidationLabelValue = "single-machine"
)

var (
	deprovisioningDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: deprovisioningSubsystem,
			Name:      "evaluation_duration_seconds",
			Help:      "Duration of the deprovisioning evaluation process in seconds.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{"method"},
	)
	deprovisioningReplacementNodeInitializedHistogram = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: deprovisioningSubsystem,
			Name:      "replacement_machine_initialized_seconds",
			Help:      "Amount of time required for a replacement machine to become initialized.",
			Buckets:   metrics.DurationBuckets(),
		})
	deprovisioningActionsPerformedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: deprovisioningSubsystem,
			Name:      "actions_performed",
			Help:      "Number of deprovisioning actions performed. Labeled by deprovisioner.",
		},
		[]string{actionLabel, deprovisionerLabel},
	)
	deprovisioningEligibleMachinesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: deprovisioningSubsystem,
			Name:      "eligible_machines",
			Help:      "Number of machines eligible for deprovisioning by Karpenter. Labeled by deprovisioner",
		},
		[]string{deprovisionerLabel},
	)
	deprovisioningConsolidationTimeoutsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: deprovisioningSubsystem,
			Name:      "consolidation_timeouts",
			Help:      "Number of times the Consolidation algorithm has reached a timeout. Labeled by consolidation type.",
		},
		[]string{consolidationTypeLabel},
	)
	deprovisioningReplacementNodeLaunchFailedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: deprovisioningSubsystem,
			Name:      "replacement_machine_launch_failure_counter",
			Help:      "The number of times that Karpenter failed to launch a replacement node for deprovisioning. Labeled by deprovisioner.",
		},
		[]string{deprovisionerLabel},
	)
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
			Help:      "The number of times that Karpenter failed to launch a replacement node for disruption. Labeled by disruption type.",
		},
		[]string{methodLabel, consolidationTypeLabel},
	)
	disruptionActionsPerformedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: disruptionSubsystem,
			Name:      "actions_performed_total",
			Help:      "Number of disruption methods performed. Labeled by disruption type.",
		},
		[]string{actionLabel, methodLabel, consolidationTypeLabel},
	)
	disruptionEligibleNodesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: disruptionSubsystem,
			Name:      "eligible_nodes",
			Help:      "Number of nodes eligible for disruption by Karpenter. Labeled by disruption type.",
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
