/*
Copyright 2023 The Kubernetes Authors.

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
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/robfig/cron/v3"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/ptr"
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
	// the most restrictive maxUnavailable. If left undefined,
	// this will default to one budget with a maxUnavailable to 10%.
	// +kubebuilder:validation:XValidation:message="'crontab' must be set with 'duration'",rule="!self.all(x, (has(x.crontab) && !has(x.duration)) || (!has(x.crontab) && has(x.duration)))"
	// +kubebuilder:default:={{maxUnavailable: "10%"}}
	// +kubebuilder:validation:MaxItems=50
	// +optional
	Budgets []Budget `json:"budgets,omitempty" hash:"ignore"`
}

// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
	// MaxUnavailable dictates how many NodeClaims owned by this NodePool
	// can be terminating at once. It must be set.
	// This only considers NodeClaims with the karpenter.sh/disruption taint.
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:Pattern=`^(\d{1,3}%)|(\d+)$`
	// +kubebuilder:default:="10%"
	MaxUnavailable intstr.IntOrString `json:"maxUnavailable" hash:"ignore"`
	// Crontab specifies when a budget begins being active,
	// using the upstream cronjob syntax. If omitted, the budget is always active.
	// Currently timezones are not supported.
	// This is required if Duration is set.
	// +kubebuilder:validation:Pattern:=`^(@(annually|yearly|monthly|weekly|daily|midnight|hourly))|((.+)\s(.+)\s(.+)\s(.+)\s(.+))$`
	// +optional
	Crontab *string `json:"crontab,omitempty" hash:"ignore"`
	// Duration determines how long a Budget is active since each Crontab hit.
	// Only minutes and hours are accepted, as cron does not work in seconds.
	// If omitted, the budget is always active.
	// This is required if Crontab is set.
	// +kubebuilder:validation:Pattern=`^(([0-9]+(m|h))+)|(Never)$`
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

	Spec   NodePoolSpec   `json:"spec,omitempty"`
	Status NodePoolStatus `json:"status,omitempty"`
}

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

// OrderByWeight orders the nodepools in the NodePoolList
// by their priority weight in-place
func (pl *NodePoolList) OrderByWeight() {
	sort.Slice(pl.Items, func(a, b int) bool {
		return ptr.Int32Value(pl.Items[a].Spec.Weight) > ptr.Int32Value(pl.Items[b].Spec.Weight)
	})
}

// GetAllowedDisruptions returns the minimum allowed disruptions across all disruption budgets for a given node pool.
// This returns two values as the resolved value for a percent depends on the number of current node claims.
func (in *NodePool) GetAllowedDisruptions(ctx context.Context, c clock.Clock) (intstr.IntOrString, intstr.IntOrString, error) {
	vals := make([]intstr.IntOrString, len(in.Spec.Disruption.Budgets))
	errs := make([]error, len(in.Spec.Disruption.Budgets))
	workqueue.ParallelizeUntil(ctx, len(in.Spec.Disruption.Budgets), len(in.Spec.Disruption.Budgets), func(i int) {
		val, err := in.Spec.Disruption.Budgets[i].GetAllowedDisruptions(c)
		if err != nil {
			errs[i] = fmt.Errorf("invalid budget %s, %w", lo.FromPtr(in.Spec.Disruption.Budgets[i].Crontab), err)
		}
		vals[i] = val
	})
	if err := multierr.Combine(errs...); err != nil {
		return intstr.IntOrString{}, intstr.IntOrString{}, err
	}
	minIntVal, minPercentVal := math.MaxInt64, math.MaxInt64
	for i := range vals {
		val := vals[i]
		// The crontab wasn't active
		if val.IntVal == -1 {
			continue
		}
		// This returns the percent value if it's a string, and the raw value if it's an int.
		temp, err := intstr.GetScaledValueFromIntOrPercent(lo.ToPtr(val), 100, false)
		if err != nil {
			// Should almost never happen since this is validated when the nodepool is applied
			return intstr.IntOrString{}, intstr.IntOrString{}, fmt.Errorf("getting intstr scaled value, %w", err)
		}
		if val.Type == intstr.Int {
			minIntVal = lo.Ternary(temp < minIntVal, temp, minIntVal)
		} else {
			minPercentVal = lo.Ternary(temp < minPercentVal, temp, minPercentVal)
		}
	}
	// return the values, defaulting to -1 if the value is MaxInt64
	return intstr.FromInt(lo.Ternary(minIntVal == math.MaxInt64, -1, minIntVal)), intstr.FromString(fmt.Sprintf("%d%%", lo.Ternary(minPercentVal == math.MaxInt64, -1, minPercentVal))), nil
}

// GetAllowedDisruptions returns an intstr.IntOrString that can be used a comparison
// for calculating if a disruption action is allowed. It returns an error if the
// crontab is invalid. This returns -1 if the value is unbounded.
func (b *Budget) GetAllowedDisruptions(c clock.Clock) (intstr.IntOrString, error) {
	active, err := b.IsActive(c)
	if err != nil {
		return intstr.IntOrString{}, err
	}
	return lo.Ternary(active, b.MaxUnavailable, intstr.FromInt(-1)), nil
}

// IsActive takes a clock as input and returns if a budget is active.
// It walks back in time the time.Duration associated with the crontab,
// and checks if the next time the schedule will hit is before the current time.
// If the last crontab hit is exactly the duration in the past, this means the
// schedule is active, as any more crontab hits in between would only extend this
// window. This ensures that any previous crontab hits for a schedule are considered.
func (b *Budget) IsActive(c clock.Clock) (bool, error) {
	if b.Crontab == nil && b.Duration == nil {
		return true, nil
	}
	schedule, err := cron.ParseStandard(lo.FromPtr(b.Crontab))
	if err != nil {
		return false, fmt.Errorf("parsing crontab, %w", err)
	}
	// Walk back in time for the duration associated with the crontab
	checkPoint := c.Now().Add(-lo.FromPtr(b.Duration).Duration)
	nextHit := schedule.Next(checkPoint)
	return !nextHit.After(c.Now()), nil
}
