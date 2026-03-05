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
	strategyLabel            = "strategy"
	resultLabel              = "result"
)

var (
	NodesRankedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "nodes_ranked_total",
			Help:      "Number of nodes ranked in total by the pod deletion cost controller. Labeled by ranking strategy.",
		},
		[]string{strategyLabel},
	)
	PodsUpdatedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "pods_updated_total",
			Help:      "Number of pod deletion cost annotations updated in total. Labeled by result (success, skipped_customer_managed, error).",
		},
		[]string{resultLabel},
	)
	RankingDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "ranking_duration_seconds",
			Help:      "Duration of node ranking computation in seconds. Labeled by ranking strategy.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{strategyLabel},
	)
	AnnotationDurationSeconds = opmetrics.NewPrometheusHistogram(
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
	SkippedNoChangesTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "skipped_no_changes_total",
			Help:      "Number of reconcile loops skipped due to no changes detected in cluster state.",
		},
		[]string{},
	)
)
