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

package deletioncost

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	podDeletionCostSubsystem = "pod_deletion_cost"
	resultLabel              = "result"
)

// noLabels is shared by all label-less metric calls so we don't allocate an
// empty map on every increment.
var noLabels = map[string]string{}

var (
	// RFC §"Observability" calls for a gauge (current footprint), not a
	// monotonic total.
	nodesRanked = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "nodes_ranked",
			Help:      "Number of nodes ranked in the most recent reconcile cycle by the pod deletion cost controller.",
		},
		[]string{},
	)
	podsUpdatedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "pods_updated_total",
			Help:      "Number of pod deletion cost annotations updated in total. Labeled by result (updated, skipped_unchanged, error). The error label counts per-pod patch failures.",
		},
		[]string{resultLabel},
	)
	rankingDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "ranking_duration_seconds",
			Help:      "Duration of node ranking computation in seconds.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{},
	)
	// Per-pod queue-reconcile duration. Previously per-cycle when
	// UpdatePodDeletionCosts ran synchronously; after the queue swap this
	// measures each Queue.Reconcile call (single-pod write, retry, or skip).
	annotationDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "annotation_duration_seconds",
			Help:      "Duration of a single pod annotation update operation in seconds.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{},
	)
	reconcileSkippedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "reconcile_skipped_total",
			Help:      "Number of reconcile loops skipped due to no changes detected in cluster state.",
		},
		[]string{},
	)
)
