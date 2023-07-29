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

package deprovisioning

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/aws/karpenter-core/pkg/metrics"
)

func init() {
	crmetrics.Registry.MustRegister(deprovisioningDurationHistogram)
	crmetrics.Registry.MustRegister(deprovisioningReplacementNodeInitializedHistogram)
	crmetrics.Registry.MustRegister(deprovisioningActionsPerformedCounter)
	crmetrics.Registry.MustRegister(deprovisioningEligibleMachinesGauge)
}

const (
	deprovisioningSubsystem = "deprovisioning"
	deprovisionerLabel      = "deprovisioner"
	actionLabel             = "action"
	consolidationType       = "consolidationType"
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
		[]string{"method"})
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
			Help:      "Number of times the Consolidation algorithm has reached a timeout. Labeled by consolidationType.",
		},
		[]string{consolidationType},
	)
)
