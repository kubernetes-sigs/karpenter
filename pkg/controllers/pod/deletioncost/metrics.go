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

var (
	ResultUpdated = opmetrics.Value{
		Name: "updated",
		Help: "The annotation was patched to the planned value.",
	}
	ResultSkippedUnchanged = opmetrics.Value{
		Name: "skipped_unchanged",
		Help: "The pod already carried the planned annotation, so no write was issued.",
	}
	ResultSkippedNotFound = opmetrics.Value{
		Name: "skipped_notfound",
		Help: "The pod was gone before the write landed.",
	}
	ResultSkippedConflict = opmetrics.Value{
		Name: "skipped_conflict",
		Help: "Another writer won the race; the next reconcile re-observes the pod.",
	}
	ResultError = opmetrics.Value{
		Name: "error",
		Help: "The write failed with a retryable API error and will be retried.",
	}
)

var Result = opmetrics.Label{
	Name:   resultLabel,
	Help:   "Outcome of the pod-deletion-cost annotation write.",
	Values: []opmetrics.Value{ResultUpdated, ResultSkippedUnchanged, ResultSkippedNotFound, ResultSkippedConflict, ResultError},
}

var (
	nodesWithPendingAnnotationWrites = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "nodes_with_pending_annotation_writes",
			Help:      "Number of nodes with at least one pending pod-deletion-cost annotation change enqueued this cycle.",
		},
		[]opmetrics.Label{metrics.NodePool},
		opmetrics.Alpha,
	)
	podAnnotationWritesTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "pod_annotation_writes_total",
			Help:      "Number of pod-deletion-cost annotation write attempts. Labeled by outcome.",
		},
		[]opmetrics.Label{Result},
		opmetrics.Alpha,
	)
)
