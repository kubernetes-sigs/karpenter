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
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

func init() {
	crmetrics.Registry.MustRegister(SimulationDurationSeconds, QueueDepth)
}

const (
	controllerLabel   = "controller"
	schedulingIDLabel = "scheduling_id"
)

var (
	SimulationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: "provisioner",
			Name:      "scheduling_simulation_duration_seconds",
			Help:      "Duration of scheduling simulations used for deprovisioning and provisioning in seconds.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{
			controllerLabel,
		},
	)
	QueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: "provisioner",
			Name:      "scheduling_queue_depth",
			Help:      "The number of pods currently waiting to be scheduled.",
		},
		[]string{
			controllerLabel,
			schedulingIDLabel,
		},
	)
)
