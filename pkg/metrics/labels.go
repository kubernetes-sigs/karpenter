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
	pmetrics "github.com/awslabs/operatorpkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Label is the source-of-truth description of a Prometheus metric label (a
// "dimension"). Declaring dimensions as Labels lets the metrics documentation
// generator (hack/docs/metrics_gen) emit per-dimension help text and, where the
// set of possible values is known and stable, the list of values.
//
// The type is defined in operatorpkg and aliased here so that Karpenter and
// operatorpkg describe their dimensions with a single, consistent type.
//
// Conventions (see AGENTS.md):
//   - Always use a Label to describe a metric dimension; never a bare string
//     literal in the metric's label-names slice.
//   - Values MUST always be a list of consts, never magic strings.
//   - Before adding a new Label, check whether an existing one already describes
//     the dimension you need and reference it instead.
type Label = pmetrics.Label

// Value documents one of the stable values a metric dimension (Label) can take.
// Like Label, it is aliased from operatorpkg so the type is shared.
type Value = pmetrics.Value

// Label-name constants. These are kept as standalone constants (in addition to
// the Label vars below) so that existing metric declarations that reference them
// continue to compile unchanged.
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
	NodePool = Label{
		Name: NodePoolLabel,
		Help: "The name of the NodePool that owns the resource.",
	}
	Reason = Label{
		Name: ReasonLabel,
		// The concrete value set is metric-specific, so this documents the common
		// cases rather than enumerating a fixed Values list.
		Help: "Why the action was taken. Values are metric-specific: create/delete " +
			"counters use `provisioned`, `expired`, or `unhealthy`; disruption metrics " +
			"use the disruption reason such as `underutilized`, `empty`, `drifted`, or " +
			"`expired`; cloud-provider failure metrics use the provider error reason.",
	}
	ResourceType = Label{
		Name: ResourceTypeLabel,
		Help: "The Kubernetes resource type, e.g. `cpu`, `memory`, `pods`.",
	}
	CapacityType = Label{
		Name: CapacityTypeLabel,
		Help: "The capacity type of the instance.",
		Values: []Value{
			{Name: v1.CapacityTypeOnDemand, Help: "On-demand capacity."},
			{Name: v1.CapacityTypeSpot, Help: "Spot capacity, which can be reclaimed by the cloud provider."},
			{Name: v1.CapacityTypeReserved, Help: "Reserved capacity, backed by a capacity reservation."},
		},
	}
	Zone = Label{
		Name: ZoneLabel,
		Help: "The availability zone of the instance.",
	}
	MinValuesRelaxed = Label{
		Name: MinValuesRelaxedLabel,
		Help: "Whether minValues requirements were relaxed to satisfy scheduling.",
	}
	ConsolidationPolicy = Label{
		Name: ConsolidationPolicyLabel,
		Help: "The NodePool consolidation policy in effect.",
		Values: []Value{
			{Name: string(v1.ConsolidationPolicyWhenEmpty), Help: "Consolidate only nodes with no workload pods."},
			{Name: string(v1.ConsolidationPolicyWhenEmptyOrUnderutilized), Help: "Consolidate empty nodes and underutilized nodes."},
			{Name: string(v1.ConsolidationPolicyBalanced), Help: "Consolidate using the balanced algorithm."},
		},
	}
	TerminationMode = Label{
		Name: TerminationModeLabel,
		Help: "The termination mode used to disrupt the node.",
		Values: []Value{
			{Name: TerminationModeGraceful, Help: "Graceful termination that respects the node's disruption budget and drains pods."},
			{Name: TerminationModeEventual, Help: "Eventual termination once the node's terminationGracePeriod elapses."},
			{Name: TerminationModeForceful, Help: "Forceful termination that deletes the node immediately."},
		},
	}
	Controller = Label{
		Name: ControllerLabel,
		Help: "The name of the controller that emitted the metric.",
	}
)
