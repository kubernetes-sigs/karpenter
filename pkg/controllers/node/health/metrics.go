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

package health

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	ImageID   = "image_id"
	Condition = "condition"
	Gate      = "gate"

	GateNodePool = "nodepool"
	GateCluster  = "cluster"
)

var (
	NodeClaimsUnhealthyDisruptedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeClaimSubsystem,
			Name:      "unhealthy_disrupted_total",
			Help:      "Number of unhealthy nodeclaims disrupted in total by Karpenter. Labeled by condition on the node was disrupted, the owning nodepool, and the image ID.",
		},
		[]string{
			Condition,
			metrics.NodePoolLabel,
			metrics.CapacityTypeLabel,
			ImageID,
		},
	)
	NodeRepairBlockedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_blocked_total",
			Help:      "Number of node repair evaluations deferred by a NodePool or cluster health gate. Repeated reconciles are counted as repeated deferrals.",
		},
		[]string{metrics.NodePoolLabel, Gate},
	)
	NodeRepairUnhealthyDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_unhealthy_duration_seconds",
			Help:      "Time in seconds from the triggering unhealthy condition transition until Karpenter successfully submitted the NodeClaim deletion.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]string{metrics.NodePoolLabel, Condition},
	)
)
