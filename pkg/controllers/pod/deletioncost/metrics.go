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

// Well-known `result` dimension values. These are metric-only values, so they
// are first-class opmetrics.Value vars: the value string and its documentation
// live in one place and emission sites refer to it by .Name.
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

// Result is the `result` dimension for the annotation-write counter. Exported so
// metric-assertion specs in the external test package name the same values the
// emission sites use.
var Result = opmetrics.Label{
	Name:   resultLabel,
	Help:   "Outcome of the pod-deletion-cost annotation write.",
	Values: []opmetrics.Value{ResultUpdated, ResultSkippedUnchanged, ResultSkippedNotFound, ResultSkippedConflict, ResultError},
}

var (
	// nodes_with_pending_annotation_writes counts nodes with at least one pending
	// pod-deletion-cost annotation change enqueued this cycle, partitioned by
	// nodepool. A node only lands here when one of its pods would actually change
	// annotation, so the gauge tracks enqueued work rather than nodes examined.
	// Reset each cycle so pools whose count drops to zero do not linger at their
	// prior value.
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
	// pod_annotation_writes_total counts write attempts, not successes. The
	// `result` dimension carries the updated-vs-skipped-vs-error split, matching
	// the neutral naming of voluntary_disruption_decisions_total.
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
