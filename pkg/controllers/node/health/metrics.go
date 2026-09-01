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
	ImageIDLabel   = "image_id"
	ConditionLabel = "condition"
)

// Metric dimensions specific to node health / repair.
var (
	Condition = opmetrics.Label{
		Name: ConditionLabel,
		Help: "The node status condition type that failed the repair health check and triggered disruption.",
	}
	ImageID = opmetrics.Label{
		Name: ImageIDLabel,
		Help: "The image ID of the node that was disrupted.",
	}
)

var NodeClaimsUnhealthyDisruptedTotal = opmetrics.NewPrometheusCounter(
	crmetrics.Registry,
	prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: metrics.NodeClaimSubsystem,
		Name:      "unhealthy_disrupted_total",
		Help:      "Number of unhealthy nodeclaims disrupted in total by Karpenter. Labeled by the condition the node was disrupted on, the owning nodepool, the capacity type, and the image ID.",
	},
	[]opmetrics.Label{
		Condition,
		metrics.NodePool,
		metrics.CapacityType,
		ImageID,
	},
)
