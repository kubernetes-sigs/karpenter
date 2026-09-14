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
	// nodes_ranked is a gauge of nodes annotated in the most recent reconcile
	// cycle, partitioned by nodepool. Reset each cycle so pools whose count
	// drops to zero don't linger at their prior value.
	nodesRanked = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "nodes_ranked",
			Help:      "Number of nodes ranked in the most recent reconcile cycle by the pod deletion cost controller.",
		},
		[]opmetrics.Label{metrics.NodePool},
	)
	podsUpdatedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "pods_updated_total",
			Help:      "Number of pod deletion cost annotations updated in total. Labeled by result (updated, skipped_unchanged, skipped_notfound, skipped_conflict, error). The error label counts per-pod patch failures; skipped_notfound covers pods that vanished before write; skipped_conflict covers writes lost to a racing writer.",
		},
		[]opmetrics.Label{{Name: resultLabel, Help: "Outcome of the annotation write (updated, skipped_unchanged, skipped_notfound, skipped_conflict, error)."}},
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
		[]opmetrics.Label{},
	)
)
