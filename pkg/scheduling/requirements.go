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

package scheduling

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/utils/functional"
)

// Requirements are an efficient set representation under the hood. Since its underlying
// types are slices and maps, this type should not be used as a pointer.
type Requirements map[string]*Requirement

func NewRequirements(requirements ...*Requirement) Requirements {
	r := Requirements{}
	for _, requirement := range requirements {
		r.Add(requirement)
	}
	return r
}

// NewRequirements constructs requirements from NodeSelectorRequirements
func NewNodeSelectorRequirements(requirements ...v1.NodeSelectorRequirement) Requirements {
	r := NewRequirements()
	for _, requirement := range requirements {
		r.Add(NewRequirement(requirement.Key, requirement.Operator, requirement.Values...))
	}
	return r
}

// NewLabelRequirements constructs requirements from labels
func NewLabelRequirements(labels map[string]string) Requirements {
	requirements := NewRequirements()
	for key, value := range labels {
		requirements.Add(NewRequirement(key, v1.NodeSelectorOpIn, value))
	}
	return requirements
}

// NewPodRequirements constructs requirements from a pod and treats any preferred requirements as required.
func NewPodRequirements(pod *v1.Pod) Requirements {
	return newPodRequirements(pod, podRequirementTypeAll)
}

// NewStrictPodRequirements constructs requirements from a pod and only includes true requirements (not preferences).
func NewStrictPodRequirements(pod *v1.Pod) Requirements {
	return newPodRequirements(pod, podRequirementTypeRequiredOnly)
}

type podRequirementType byte

const (
	podRequirementTypeAll = iota
	podRequirementTypeRequiredOnly
)

func newPodRequirements(pod *v1.Pod, typ podRequirementType) Requirements {
	requirements := NewLabelRequirements(pod.Spec.NodeSelector)
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return requirements
	}
	if typ == podRequirementTypeAll {
		// The legal operators for pod affinity and anti-affinity are In, NotIn, Exists, DoesNotExist.
		// Select heaviest preference and treat as a requirement. An outer loop will iteratively unconstrain them if unsatisfiable.
		if preferred := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution; len(preferred) > 0 {
			sort.Slice(preferred, func(i int, j int) bool { return preferred[i].Weight > preferred[j].Weight })
			requirements.Add(NewNodeSelectorRequirements(preferred[0].Preference.MatchExpressions...).Values()...)
		}
	}

	// Select first requirement. An outer loop will iteratively remove OR requirements if unsatisfiable
	if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil &&
		len(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) > 0 {
		requirements.Add(NewNodeSelectorRequirements(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions...).Values()...)
	}
	return requirements
}

// HasPreferredNodeAffinity returns true if the pod has a preferred node affinity term
func HasPreferredNodeAffinity(p *v1.Pod) bool {
	if p == nil {
		return false
	}
	return p.Spec.Affinity != nil && p.Spec.Affinity.NodeAffinity != nil && len(p.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0
}

func (r Requirements) NodeSelectorRequirements() []v1.NodeSelectorRequirement {
	return lo.Map(lo.Values(r), func(req *Requirement, _ int) v1.NodeSelectorRequirement {
		return req.NodeSelectorRequirement()
	})
}

// Add requirements to provided requirements. Mutates existing requirements
func (r Requirements) Add(requirements ...*Requirement) {
	for _, requirement := range requirements {
		if existing, ok := r[requirement.Key]; ok {
			requirement = requirement.Intersection(existing)
		}
		r[requirement.Key] = requirement
	}
}

// Keys returns unique set of the label keys from the requirements
func (r Requirements) Keys() sets.Set[string] {
	keys := sets.New[string]()
	for key := range r {
		keys.Insert(key)
	}
	return keys
}

func (r Requirements) Values() []*Requirement {
	return lo.Values(r)
}

func (r Requirements) Has(key string) bool {
	_, ok := r[key]
	return ok
}

func (r Requirements) Get(key string) *Requirement {
	if _, ok := r[key]; !ok {
		// If not defined, allow any values with the exists operator
		return NewRequirement(key, v1.NodeSelectorOpExists)
	}
	return r[key]
}

type CompatabilityOptions struct {
	AllowUndefined sets.Set[string]
}

<<<<<<< Updated upstream
=======

>>>>>>> Stashed changes
var AllowUndefinedWellKnownLabels = func(options CompatabilityOptions) CompatabilityOptions {
	options.AllowUndefined = v1beta1.WellKnownLabels
	return options
}

// Compatible ensures the provided requirements can loosely be met.
func (r Requirements) Compatible(requirements Requirements, options ...functional.Option[CompatabilityOptions]) (errs error) {
	opts := functional.ResolveOptions(options...)
	// Custom Labels must intersect, but if not defined are denied.
	for key := range requirements.Keys().Difference(opts.AllowUndefined) {
		if operator := requirements.Get(key).Operator(); r.Has(key) || operator == v1.NodeSelectorOpNotIn || operator == v1.NodeSelectorOpDoesNotExist {
			continue
		}
		errs = multierr.Append(errs, fmt.Errorf("label %q does not have known values%s", key, labelHint(r, key, opts.AllowUndefined)))
	}
	// Well Known Labels must intersect, but if not defined, are allowed.
	return multierr.Append(errs, r.Intersects(requirements))
}

// editDistance is an implementation of edit distance from Algorithms/DPV
func editDistance(s, t string) int {
	min := func(a, b, c int) int {
		m := a
		if b < m {
			m = b
		}
		if c < m {
			m = c
		}
		return m
	}

	m := len(s)
	n := len(t)
	if m == 0 {
		return n
	}
	if n == 0 {
		return m
	}
	prevRow := make([]int, n)
	curRow := make([]int, n)
	for j := 1; j < n; j++ {
		prevRow[j] = j
	}
	for i := 1; i < m; i++ {
		for j := 1; j < n; j++ {
			diff := 0
			if s[i] != t[j] {
				diff = 1
			}
			curRow[j] = min(prevRow[j]+1, curRow[j-1]+1, prevRow[j-1]+diff)
		}
		prevRow, curRow = curRow, prevRow
	}
	return prevRow[n-1]
}

func getSuffix(key string) string {
	before, after, found := strings.Cut(key, "/")
	return lo.Ternary(found, after, before)
}

func labelHint(r Requirements, key string, allowedUndefined sets.Set[string]) string {
	for wellKnown := range allowedUndefined {
		if strings.Contains(wellKnown, key) || editDistance(key, wellKnown) < len(wellKnown)/5 {
			return fmt.Sprintf(" (typo of %q?)", wellKnown)
		}
		if strings.HasSuffix(wellKnown, getSuffix(key)) {
			return fmt.Sprintf(" (typo of %q?)", wellKnown)
		}
	}
	for existing := range r {
		if strings.Contains(existing, key) || editDistance(key, existing) < len(existing)/5 {
			return fmt.Sprintf(" (typo of %q?)", existing)
		}
		if strings.HasSuffix(existing, getSuffix(key)) {
			return fmt.Sprintf(" (typo of %q?)", existing)
		}
	}
	return ""
}

// Intersects returns errors if the requirements don't have overlapping values, undefined keys are allowed
func (r Requirements) Intersects(requirements Requirements) (errs error) {
	for key := range r.Keys().Intersection(requirements.Keys()) {
		existing := r.Get(key)
		incoming := requirements.Get(key)
		// There must be some value, except
		if existing.Intersection(incoming).Len() == 0 {
			// where the incoming requirement has operator { NotIn, DoesNotExist }
			if operator := incoming.Operator(); operator == v1.NodeSelectorOpNotIn || operator == v1.NodeSelectorOpDoesNotExist {
				// and the existing requirement has operator { NotIn, DoesNotExist }
				if operator := existing.Operator(); operator == v1.NodeSelectorOpNotIn || operator == v1.NodeSelectorOpDoesNotExist {
					continue
				}
			}
			errs = multierr.Append(errs, fmt.Errorf("key %s, %s not in %s", key, incoming, existing))
		}
	}
	return errs
}

func (r Requirements) Labels() map[string]string {
	labels := map[string]string{}
	for key, requirement := range r {
		if !v1beta1.IsRestrictedNodeLabel(key) {
			if value := requirement.Any(); value != "" {
				labels[key] = value
			}
		}
	}
	return labels
}

func (r Requirements) String() string {
	requirements := lo.Reject(r.Values(), func(requirement *Requirement, _ int) bool {
		return v1beta1.RestrictedLabels.Has(requirement.Key)
	})
	stringRequirements := lo.Map(requirements, func(requirement *Requirement, _ int) string { return requirement.String() })
	slices.Sort(stringRequirements)
	return strings.Join(stringRequirements, ", ")
}
