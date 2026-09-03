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

package scheduling

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	// ControllerLabel is an alias for the shared controller dimension. Its
	// canonical description (help text) lives on metrics.Controller.
	ControllerLabel    = metrics.ControllerLabel
	schedulingIDLabel  = "scheduling_id"
	schedulerSubsystem = "scheduler"
)

// SchedulingID describes the scheduling_id dimension. It is co-located here
// because its value is a package-local runtime UUID.
var SchedulingID = opmetrics.Label{
	Name: schedulingIDLabel,
	Help: "A unique identifier for a scheduling simulation run.",
}

var (
	DurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: schedulerSubsystem,
			Name:      "scheduling_duration_seconds",
			Help:      "Duration of scheduling simulations used for deprovisioning and provisioning in seconds.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]opmetrics.Label{
			metrics.Controller,
		},
	)
	QueueDepth = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: schedulerSubsystem,
			Name:      "queue_depth",
			Help:      "The number of pods currently waiting to be scheduled.",
		},
		[]opmetrics.Label{
			metrics.Controller,
			SchedulingID,
		},
	)
	UnfinishedWorkSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: schedulerSubsystem,
			Name:      "unfinished_work_seconds",
			Help:      "How many seconds of work has been done that is in progress and hasn't been observed by scheduling_duration_seconds.",
		},
		[]opmetrics.Label{
			metrics.Controller,
			SchedulingID,
		},
	)
	IgnoredPodCount = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: schedulerSubsystem,
			Name:      "ignored_pods_count",
			Help:      "Number of pods ignored during scheduling by Karpenter",
		},
		[]opmetrics.Label{},
	)
	UnschedulablePodsCount = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: schedulerSubsystem,
			Name:      "unschedulable_pods_count",
			Help:      "The number of unschedulable Pods.",
		},
		[]opmetrics.Label{
			metrics.Controller,
		},
	)
	PendingPodsByEffectiveZone = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: schedulerSubsystem,
			Name:      "pending_pods_by_effective_zone_count",
			Help:      "Pending pods dimensioned by effective zone constraint, or the intersection of pod-level zone signals, volume topology (PVC zones), and topology constraints. Values: specific zone name (e.g., 'us-west-2a'), 'flexible' (multiple zones), or 'none' (no valid intersection).",
		},
		[]opmetrics.Label{
			metrics.Controller,
			metrics.Zone,
		},
	)
)
