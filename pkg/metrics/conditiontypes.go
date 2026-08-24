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
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// ConditionTypeValues documents, per object Kind, the status condition types the
// object sets. This is the stable value set of the `type` dimension on that
// object's `operator_<kind>_status_condition_*` metrics (emitted by operatorpkg's
// status controller). The metrics docs generator reads this map to document the
// `type` dimension per object.
//
// It also serves as a single cheat-sheet of which status conditions Karpenter
// reads and sets, and why. Keys are the object Kind (matching the type parameter
// of status.NewController[T]); every value's Name references a ConditionType*
// const so the set cannot drift from the API definitions.
var ConditionTypeValues = map[string][]Value{
	"NodeClaim": {
		{Name: v1.ConditionTypeLaunched, Help: "The instance backing the NodeClaim has been launched with the cloud provider."},
		{Name: v1.ConditionTypeRegistered, Help: "The launched node has registered with the cluster."},
		{Name: v1.ConditionTypeInitialized, Help: "The registered node has finished initializing and is ready for workloads."},
		{Name: v1.ConditionTypeConsolidatable, Help: "The NodeClaim is currently eligible for consolidation."},
		{Name: v1.ConditionTypeDrifted, Help: "The NodeClaim has drifted from its desired specification."},
		{Name: v1.ConditionTypeDrained, Help: "The node has been drained of pods during termination."},
		{Name: v1.ConditionTypeVolumesDetached, Help: "The node's volumes have been detached during termination."},
		{Name: v1.ConditionTypeInstanceTerminating, Help: "The backing instance is terminating."},
		{Name: v1.ConditionTypeConsistentStateFound, Help: "A consistent instance state was observed before disruption."},
		{Name: v1.ConditionTypeDisruptionReason, Help: "Set while the NodeClaim is being disrupted; records the disruption reason."},
	},
	"NodePool": {
		{Name: v1.ConditionTypeValidationSucceeded, Help: "The runtime-based configuration is valid for this NodePool."},
		{Name: v1.ConditionTypeNodeClassReady, Help: "The underlying NodeClass was resolved and is reporting as Ready."},
		{Name: v1.ConditionTypeNodeRegistrationHealthy, Help: "No misconfiguration is preventing successful node launch/registration."},
	},
}
