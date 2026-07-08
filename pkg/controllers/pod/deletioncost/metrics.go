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

// noLabels is shared by all label-less metric calls so we don't allocate a
// fresh empty map on every increment.
var noLabels = map[string]string{}

var (
	// nodes_ranked is a gauge of the number of nodes ranked in the most
	// recent reconcile cycle. RFC §"Observability" calls for a gauge so
	// operators can see the current managed-node footprint, not a monotonic
	// total.
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
			Help:      "Number of pod deletion cost annotations updated in total. Labeled by result (updated, skipped_unchanged, error). The error label counts per-pod patch failures only; per-node list-fetch failures are counted separately on nodes_errored_total.",
		},
		[]string{resultLabel},
	)
	// nodesErroredTotal counts nodes whose pod-list fetch failed in a reconcile
	// cycle, so the controller never attempted to patch that node's pods. Kept
	// as a separate counter (not another result-label on pods_updated_total) so
	// operators can distinguish a flaky apiserver-list path from a flaky
	// per-pod patch path.
	nodesErroredTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "nodes_errored_total",
			Help:      "Number of nodes whose pod-list fetch failed during pod deletion cost reconcile. Per-pod patch failures are counted on pods_updated_total{result=error} instead.",
		},
		[]string{},
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
	annotationDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "annotation_duration_seconds",
			Help:      "Duration of pod annotation update operations in seconds.",
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
