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
}

const deprovisioningSubsystem = "deprovisioning"

var deprovisioningDurationHistogram = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: deprovisioningSubsystem,
		Name:      "evaluation_duration_seconds",
		Help:      "Duration of the deprovisioning evaluation process in seconds.",
		Buckets:   metrics.DurationBuckets(),
	},
	[]string{"method"},
)

var deprovisioningReplacementNodeInitializedHistogram = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: deprovisioningSubsystem,
		Name:      "replacement_machine_initialized_seconds",
		Help:      "Amount of time required for a replacement machine to become initialized.",
		Buckets:   metrics.DurationBuckets(),
	})

var deprovisioningActionsPerformedCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: deprovisioningSubsystem,
		Name:      "actions_performed",
		Help:      "Number of deprovisioning actions performed. Labeled by action.",
	},
	[]string{"action"},
)
