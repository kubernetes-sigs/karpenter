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
	ConditionLabel = "condition"
	ImageIDLabel   = "image_id"
)

var (
	RepairCondition = opmetrics.Label{
		Name: ConditionLabel,
		Help: "The node status condition type that triggered node repair disruption.",
	}
	ImageID = opmetrics.Label{
		Name: ImageIDLabel,
		Help: "The image ID of the node that was disrupted.",
	}
	// NodeClaimsUnhealthyDisruptedTotal preserves the per-condition/per-image breakdown the retired node.health
	// controller emitted, which the reason-labeled karpenter_nodeclaims_disrupted_total loses. Labeled by the repair
	// condition, the owning NodePool, the capacity type, and the image ID. Both repair paths (the disruption method for
	// initialized nodes and the uninitialized-node controller) emit it.
	NodeClaimsUnhealthyDisruptedTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeClaimSubsystem,
			Name:      "unhealthy_disrupted_total",
			Help:      "Number of unhealthy nodeclaims disrupted in total by node repair. Labeled by the condition the node was disrupted on, the owning nodepool, the capacity type, the image ID, and the termination mode.",
		},
		[]opmetrics.Label{RepairCondition, metrics.NodePool, metrics.CapacityType, ImageID, metrics.TerminationMode},
		opmetrics.Alpha,
	)
)
