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

package v1beta1

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/robfig/cron/v3"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/clock"
)

// NodePoolSpec is the top level nodepool specification. Nodepools
// launch nodes in response to pods that are unschedulable. A single nodepool
// is capable of managing a diverse set of nodes. Node properties are determined
// from a combination of nodepool and pod scheduling constraints.
type NodePoolSpec struct {
	// Template contains the template of possibilities for the provisioning logic to launch a NodeClaim with.
	// NodeClaims launched from this NodePool will often be further constrained than the template specifies.
	// +required
	Template NodeClaimTemplate `json:"template"`
	// Disruption contains the parameters that relate to Karpenter's disruption logic
	// +kubebuilder:default={"consolidationPolicy": "WhenUnderutilized", "expireAfter": "720h"}
	// +kubebuilder:validation:XValidation:message="consolidateAfter cannot be combined with consolidationPolicy=WhenUnderutilized",rule="has(self.consolidateAfter) ? self.consolidationPolicy != 'WhenUnderutilized' || self.consolidateAfter == 'Never' : true"
	// +kubebuilder:validation:XValidation:message="consolidateAfter must be specified with consolidationPolicy=WhenEmpty",rule="self.consolidationPolicy == 'WhenEmpty' ? has(self.consolidateAfter) : true"
	// +optional
	Disruption Disruption `json:"disruption"`
	// Limits define a set of bounds for provisioning capacity.
	// +optional
	Limits Limits `json:"limits,omitempty"`
	// Weight is the priority given to the nodepool during scheduling. A higher
	// numerical weight indicates that this nodepool will be ordered
	// ahead of other nodepools with lower weights. A nodepool with no weight
	// will be treated as if it is a nodepool with a weight of 0.
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=100
	// +optional
	Weight *int32 `json:"weight,omitempty"`
}

type Disruption struct {
	// ConsolidateAfter is the duration the controller will wait
	// before attempting to terminate nodes that are underutilized.
	// Refer to ConsolidationPolicy for how underutilization is considered.
	// +kubebuilder:validation:Pattern=`^(([0-9]+(s|m|h))+)|(Never)$`
	// +kubebuilder:validation:Type="string"
	// +kubebuilder:validation:Schemaless
	// +optional
	ConsolidateAfter *NillableDuration `json:"consolidateAfter,omitempty"`
	// ConsolidationPolicy describes which nodes Karpenter can disrupt through its consolidation
	// algorithm. This policy defaults to "WhenUnderutilized" if not specified
	// +kubebuilder:default:="WhenUnderutilized"
	// +kubebuilder:validation:Enum:={WhenEmpty,WhenUnderutilized}
	// +optional
	ConsolidationPolicy ConsolidationPolicy `json:"consolidationPolicy,omitempty"`
	// ExpireAfter is the duration the controller will wait
	// before terminating a node, measured from when the node is created. This
	// is useful to implement features like eventually consistent node upgrade,
	// memory leak protection, and disruption testing.
	// +kubebuilder:default:="720h"
	// +kubebuilder:validation:Pattern=`^(([0-9]+(s|m|h))+)|(Never)$`
	// +kubebuilder:validation:Type="string"
	// +kubebuilder:validation:Schemaless
	// +optional
	ExpireAfter NillableDuration `json:"expireAfter"`
	// Budgets is a list of Budgets.
	// If there are multiple active budgets, Karpenter uses
	// the most restrictive value. If left undefined,
	// this will default to one budget with a value to 10%.
	// +kubebuilder:validation:XValidation:message="'schedule' must be set with 'duration'",rule="self.all(x, has(x.schedule) == has(x.duration))"
	// +kubebuilder:default:={{nodes: "10%"}}
	// +kubebuilder:validation:MaxItems=50
	// +optional
	Budgets []Budget `json:"budgets,omitempty" hash:"ignore"`
}

// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
	// Nodes dictates the maximum number of NodeClaims owned by this NodePool
	// that can be terminating at once. This is calculated by counting nodes that
	// have a deletion timestamp set, or are actively being deleted by Karpenter.
	// This field is required when specifying a budget.
	// This cannot be of type intstr.IntOrString since kubebuilder doesn't support pattern
	// checking for int nodes for IntOrString nodes.
	// Ref: https://github.com/kubernetes-sigs/controller-tools/blob/55efe4be40394a288216dab63156b0a64fb82929/pkg/crd/markers/validation.go#L379-L388
	// +kubebuilder:validation:Pattern:="^((100|[0-9]{1,2})%|[0-9]+)$"
	// +kubebuilder:default:="10%"
	Nodes string `json:"nodes" hash:"ignore"`
	// Schedule specifies when a budget begins being active, following
	// the upstream cronjob syntax. If omitted, the budget is always active.
	// Timezones are not supported.
	// This field is required if Duration is set.
	// +kubebuilder:validation:Pattern:=`^(@(annually|yearly|monthly|weekly|daily|midnight|hourly))|((.+)\s(.+)\s(.+)\s(.+)\s(.+))$`
	// +optional
	Schedule *string `json:"schedule,omitempty" hash:"ignore"`
	// Duration determines how long a Budget is active since each Schedule hit.
	// Only minutes and hours are accepted, as cron does not work in seconds.
	// If omitted, the budget is always active.
	// This is required if Schedule is set.
	// This regex has an optional 0s at the end since the duration.String() always adds
	// a 0s at the end.
	// +kubebuilder:validation:Pattern=`^((([0-9]+(h|m))|([0-9]+h[0-9]+m))(0s)?)$`
	// +kubebuilder:validation:Type="string"
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty" hash:"ignore"`
}

type ConsolidationPolicy string

const (
	ConsolidationPolicyWhenEmpty         ConsolidationPolicy = "WhenEmpty"
	ConsolidationPolicyWhenUnderutilized ConsolidationPolicy = "WhenUnderutilized"
)

type Limits v1.ResourceList

func (l Limits) ExceededBy(resources v1.ResourceList) error {
	if l == nil {
		return nil
	}
	for resourceName, usage := range resources {
		if limit, ok := l[resourceName]; ok {
			if usage.Cmp(limit) > 0 {
				return fmt.Errorf("%s resource usage of %v exceeds limit of %v", resourceName, usage.AsDec(), limit.AsDec())
			}
		}
	}
	return nil
}

type NodeClaimTemplate struct {
	ObjectMeta `json:"metadata,omitempty"`
	// +required
	Spec NodeClaimSpec `json:"spec"`
}

type ObjectMeta struct {
	// Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// NodePool is the Schema for the NodePools API
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=nodepools,scope=Cluster,categories=karpenter
// +kubebuilder:printcolumn:name="NodeClass",type="string",JSONPath=".spec.template.spec.nodeClassRef.name",description=""
// +kubebuilder:printcolumn:name="Weight",type="string",JSONPath=".spec.weight",priority=1,description=""
// +kubebuilder:subresource:status
type NodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   NodePoolSpec   `json:"spec"`
	Status NodePoolStatus `json:"status,omitempty"`
}

// We need to bump the NodePoolHashVersion when we make an update to the NodePool CRD under these conditions:
// 1. A field changes its default value for an existing field that is already hashed
// 2. A field is added to the hash calculation with an already-set value
// 3. A field is removed from the hash calculations
const NodePoolHashVersion = "v2"

func (in *NodePool) Hash() string {
	return fmt.Sprint(lo.Must(hashstructure.Hash(in.Spec.Template, hashstructure.FormatV2, &hashstructure.HashOptions{
		SlicesAsSets:    true,
		IgnoreZeroValue: true,
		ZeroNil:         true,
	})))
}

// NodePoolList contains a list of NodePool
// +kubebuilder:object:root=true
type NodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodePool `json:"items"`
}

// OrderByWeight orders the NodePools in the NodePoolList by their priority weight in-place.
// This priority evaluates the following things in precedence order:
//  1. NodePools that have a larger weight are ordered first
//  2. If two NodePools have the same weight, then the NodePool with the name later in the alphabet will come first
func (nl *NodePoolList) OrderByWeight() {
	sort.Slice(nl.Items, func(a, b int) bool {
		weightA := lo.FromPtr(nl.Items[a].Spec.Weight)
		weightB := lo.FromPtr(nl.Items[b].Spec.Weight)

		if weightA == weightB {
			// Order NodePools by name for a consistent ordering when sorting equal weight
			return nl.Items[a].Name > nl.Items[b].Name
		}
		return weightA > weightB
	})
}

// GetAllowedDisruptions returns an intstr.IntOrString that can be used a comparison
// for calculating if a disruption action is allowed. It returns an error if the
// schedule is invalid. This returns MAXINT if the value is unbounded.
func (in *Budget) GetAllowedDisruptions(c clock.Clock, numNodes int) (int, error) {
	active, err := in.IsActive(c)
	// If the budget is misconfigured, fail closed.
	if err != nil {
		return 0, err
	}
	if !active {
		return math.MaxInt32, nil
	}
	// This will round up to the nearest whole number. Therefore, a disruption can
	// sometimes exceed the disruption budget. This is the same as how Kubernetes
	// handles MaxUnavailable with PDBs. Take the case with 5% disruptions, but
	// 10 nodes. Karpenter will opt to allow 1 node to be disrupted, rather than
	// blocking all disruptions for this nodepool.
	res, err := intstr.GetScaledValueFromIntOrPercent(lo.ToPtr(GetIntStrFromValue(in.Nodes)), numNodes, true)
	if err != nil {
		// Should never happen since this is validated when the nodepool is applied
		// If this value is incorrectly formatted, fail closed, since we don't know what
		// they want here.
		return 0, err
	}
	return res, nil
}

// IsActive takes a clock as input and returns if a budget is active.
// It walks back in time the time.Duration associated with the schedule,
// and checks if the next time the schedule will hit is before the current time.
// If the last schedule hit is exactly the duration in the past, this means the
// schedule is active, as any more schedule hits in between would only extend this
// window. This ensures that any previous schedule hits for a schedule are considered.
func (in *Budget) IsActive(c clock.Clock) (bool, error) {
	if in.Schedule == nil && in.Duration == nil {
		return true, nil
	}
	schedule, err := cron.ParseStandard(fmt.Sprintf("TZ=UTC %s", lo.FromPtr(in.Schedule)))
	if err != nil {
		// Should only occur if there's a discrepancy
		// with the validation regex and the cron package.
		return false, fmt.Errorf("invariant violated, invalid cron %s", schedule)
	}
	// Walk back in time for the duration associated with the schedule
	checkPoint := c.Now().UTC().Add(-lo.FromPtr(in.Duration).Duration)
	nextHit := schedule.Next(checkPoint)
	return !nextHit.After(c.Now().UTC()), nil
}

func GetIntStrFromValue(str string) intstr.IntOrString {
	// If err is nil, we treat it as an int.
	if intVal, err := strconv.Atoi(str); err == nil {
		return intstr.FromInt(intVal)
	}
	return intstr.FromString(str)
}
