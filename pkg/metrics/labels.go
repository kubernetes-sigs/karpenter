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

package metrics

import (
	"strconv"
	"strings"

	opmetrics "github.com/awslabs/operatorpkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// BoolValues is the value set for a boolean dimension. The values are
// self-explanatory, so they carry no help text.
var BoolValues = []opmetrics.Value{{Name: strconv.FormatBool(true)}, {Name: strconv.FormatBool(false)}}

// reason dimension values, shared across the reason Labels below. A value that
// applies to more than one reason Label is declared once here and referenced by
// each Label that emits it.
var (
	reasonProvisioned          = opmetrics.Value{Name: ProvisionedReason, Help: "Capacity was provisioned for pending pods."}
	reasonExpired              = opmetrics.Value{Name: ExpiredReason, Help: "The Node exceeded its expiration."}
	reasonUnhealthy            = opmetrics.Value{Name: UnhealthyReason, Help: "The node failed a node-repair health check."}
	reasonGarbageCollected     = opmetrics.Value{Name: GarbageCollectedReason, Help: "The NodeClaim's backing instance was gone and it was garbage collected."}
	reasonInsufficientCapacity = opmetrics.Value{Name: InsufficientCapacityReason, Help: "The cloud provider had insufficient capacity to launch the NodeClaim."}
	reasonNodeClassNotReady    = opmetrics.Value{Name: NodeClassNotReadyReason, Help: "The NodeClaim's NodeClass was not ready."}
	reasonRegistrationTimeout  = opmetrics.Value{Name: RegistrationTimeoutReason, Help: "The NodeClaim's node did not register within the liveness timeout."}
	reasonLaunchTimeout        = opmetrics.Value{Name: LaunchTimeoutReason, Help: "The NodeClaim's backing instance did not launch within the liveness timeout."}
	reasonUnderutilized        = opmetrics.Value{Name: strings.ToLower(string(v1.DisruptionReasonUnderutilized)), Help: "The node was underutilized."}
	reasonEmpty                = opmetrics.Value{Name: strings.ToLower(string(v1.DisruptionReasonEmpty)), Help: "The node had no workload pods."}
	reasonDrifted              = opmetrics.Value{Name: strings.ToLower(string(v1.DisruptionReasonDrifted)), Help: "The node drifted from its desired specification."}
)

// disruptionReasonValues are the voluntary-disruption reasons, shared by the
// disruption metrics and the NodeClaim/Pod disruption counters.
var disruptionReasonValues = []opmetrics.Value{reasonUnderutilized, reasonEmpty, reasonDrifted}

// nodeClaimDisruptedReasonValues is the union of every reason a NodeClaim is
// disrupted: the voluntary-disruption reasons plus the involuntary paths.
var nodeClaimDisruptedReasonValues = []opmetrics.Value{
	reasonUnhealthy,
	reasonExpired,
	reasonGarbageCollected,
	reasonInsufficientCapacity,
	reasonNodeClassNotReady,
	reasonRegistrationTimeout,
	reasonLaunchTimeout,
	reasonUnderutilized,
	reasonEmpty,
	reasonDrifted,
}

// Metric dimensions and their values are described with opmetrics.Label /
// opmetrics.Value (from operatorpkg, so Karpenter and operatorpkg share one type).
// See AGENTS.md for the conventions.

// Label-name constants, referenced by metric declarations.
const (
	NodePoolLabel            = "nodepool"
	ReasonLabel              = "reason"
	ResourceTypeLabel        = "resource_type"
	CapacityTypeLabel        = "capacity_type"
	ZoneLabel                = "zone"
	MinValuesRelaxedLabel    = "min_values_relaxed"
	ConsolidationPolicyLabel = "consolidation_policy"
	TerminationModeLabel     = "termination_mode"
	ControllerLabel          = "controller"
)

// Shared core metric dimensions. Provider packages and core controllers should
// reference these rather than redeclaring the same dimension.
var (
	NodePool = opmetrics.Label{
		Name: NodePoolLabel,
		Help: "The name of the NodePool that owns the resource.",
	}
	// DisruptionReason is the `reason` dimension for voluntary-disruption metrics.
	DisruptionReason = opmetrics.Label{
		Name:   ReasonLabel,
		Help:   "The voluntary-disruption reason.",
		Values: disruptionReasonValues,
	}
	// NodeClaimCreatedReason is the `reason` dimension for the NodeClaim create
	// counter: provisioning, plus the disruption reasons (disruption creates
	// replacement NodeClaims).
	NodeClaimCreatedReason = opmetrics.Label{
		Name:   ReasonLabel,
		Help:   "Why the NodeClaim was created.",
		Values: append([]opmetrics.Value{reasonProvisioned}, disruptionReasonValues...),
	}
	// NodeClaimDisruptedReason is the `reason` dimension for the NodeClaim/Pod
	// disruption counters: the union of every path that disrupts a NodeClaim.
	NodeClaimDisruptedReason = opmetrics.Label{
		Name:   ReasonLabel,
		Help:   "Why the NodeClaim was disrupted.",
		Values: nodeClaimDisruptedReasonValues,
	}
	ResourceType = opmetrics.Label{
		Name: ResourceTypeLabel,
		Help: "The Kubernetes resource type, e.g. `cpu`, `memory`, `pods`.",
	}
	CapacityType = opmetrics.Label{
		Name: CapacityTypeLabel,
		Help: "The capacity type of the instance.",
		Values: []opmetrics.Value{
			{
				Name: v1.CapacityTypeOnDemand,
				Help: "On-demand capacity.",
			},
			{
				Name: v1.CapacityTypeSpot,
				Help: "Spot capacity, which can be reclaimed by the cloud provider.",
			},
			{
				Name: v1.CapacityTypeReserved,
				Help: "Reserved capacity, backed by a capacity reservation.",
			},
		},
	}
	Zone = opmetrics.Label{
		Name: ZoneLabel,
		Help: "The availability zone of the instance.",
	}
	MinValuesRelaxed = opmetrics.Label{
		Name:   MinValuesRelaxedLabel,
		Help:   "Whether minValues requirements were relaxed to satisfy scheduling.",
		Values: BoolValues,
	}
	ConsolidationPolicy = opmetrics.Label{
		Name: ConsolidationPolicyLabel,
		Help: "The NodePool consolidation policy in effect.",
		Values: []opmetrics.Value{
			// Emitted snake-cased: queue.go applies pretty.ToSnakeCase to the policy.
			{
				Name: pretty.ToSnakeCase(string(v1.ConsolidationPolicyWhenEmpty)),
				Help: "Consolidate only empty nodes (nodes running only pods with no disruption cost, e.g. DaemonSets).",
			},
			{
				Name: pretty.ToSnakeCase(string(v1.ConsolidationPolicyBalanced)),
				Help: "Consolidate nodes where the cost savings outweigh the disruption to running pods.",
			},
			{
				Name: pretty.ToSnakeCase(string(v1.ConsolidationPolicyWhenEmptyOrUnderutilized)),
				Help: "Consolidate any node that can be removed or replaced to reduce cost.",
			},
		},
	}
	TerminationMode = opmetrics.Label{
		Name: TerminationModeLabel,
		Help: "The termination mode used to disrupt the node.",
		Values: []opmetrics.Value{
			{
				Name: TerminationModeGraceful,
				Help: "The NodeClaim has no terminationGracePeriod, so termination respects blocking pod PDBs and the do-not-disrupt annotation.",
			},
			{
				Name: TerminationModeEventual,
				Help: "The NodeClaim has a positive terminationGracePeriod, so termination is bounded by it and overrides blocking pod PDBs and the do-not-disrupt annotation.",
			},
			{
				Name: TerminationModeForceful,
				Help: "The NodeClaim has a zero (non-positive) terminationGracePeriod, so it is terminated immediately.",
			},
		},
	}
	Controller = opmetrics.Label{
		Name: ControllerLabel,
		Help: "The name of the controller that emitted the metric.",
	}
)
