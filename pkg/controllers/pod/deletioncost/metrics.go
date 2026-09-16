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
	// nodes_ranked is a gauge of nodes with at least one pending
	// pod-deletion-cost annotation change enqueued this cycle, partitioned by
	// nodepool. Reset each cycle so pools whose count drops to zero don't
	// linger at their prior value.
	nodesRanked = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "nodes_ranked",
			Help:      "Number of nodes with at least one pending pod-deletion-cost annotation change enqueued this cycle.",
		},
		[]opmetrics.Label{metrics.NodePool},
	)
	podLabelsUpdatedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: podDeletionCostSubsystem,
			Name:      "pod_labels_updated_total",
			Help:      "Number of pod-deletion-cost annotation write attempts by outcome (updated, skipped_unchanged, skipped_notfound, skipped_conflict, error).",
		},
		[]opmetrics.Label{{Name: resultLabel, Help: "Outcome of the annotation write."}},
	)
)
