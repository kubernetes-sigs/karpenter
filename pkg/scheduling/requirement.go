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
	"math"
	"math/rand"
	"strconv"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

//go:generate go tool controller-gen object:headerFile="../../hack/boilerplate.go.txt" paths="."

// Requirement is an efficient represenatation of corev1.NodeSelectorRequirement
// +k8s:deepcopy-gen=true
type Requirement struct {
	Key                string
	complement         bool
	values             sets.Set[string]
	greaterThan        *int // Gt: value must be > N
	greaterThanOrEqual *int // Gte: value must be >= N
	lessThan           *int // Lt: value must be < N
	lessThanOrEqual    *int // Lte: value must be <= N
	MinValues          *int
}

// NewRequirementWithFlexibility constructs new requirement from the combination of key, values, minValues and the operator that
// connects the keys and values.
// nolint:gocyclo
func NewRequirementWithFlexibility(key string, operator corev1.NodeSelectorOperator, minValues *int, values ...string) *Requirement {
	if normalized, ok := v1.NormalizedLabels[key]; ok {
		key = normalized
	}

	// This is a super-common case, so optimize for it an inline everything.
	if operator == corev1.NodeSelectorOpIn {
		s := make(sets.Set[string], len(values))
		for _, value := range values {
			s[value] = sets.Empty{}
		}
		return &Requirement{
			Key:        key,
			values:     s,
			complement: false,
			MinValues:  minValues,
		}
	}

	r := &Requirement{
		Key:        key,
		values:     sets.New[string](),
		complement: true,
		MinValues:  minValues,
	}
	if operator == corev1.NodeSelectorOpIn || operator == corev1.NodeSelectorOpDoesNotExist {
		r.complement = false
	}
	if operator == corev1.NodeSelectorOpIn || operator == corev1.NodeSelectorOpNotIn {
		r.values.Insert(values...)
	}
	if operator == corev1.NodeSelectorOpGt {
		value, _ := strconv.Atoi(values[0]) // prevalidated
		r.greaterThan = &value
	}
	if operator == corev1.NodeSelectorOpLt {
		value, _ := strconv.Atoi(values[0]) // prevalidated
		r.lessThan = &value
	}
	if operator == corev1.NodeSelectorOperator(v1.NodeSelectorOpGte) {
		value, _ := strconv.Atoi(values[0]) // prevalidated
		r.greaterThanOrEqual = &value
	}
	if operator == corev1.NodeSelectorOperator(v1.NodeSelectorOpLte) {
		value, _ := strconv.Atoi(values[0]) // prevalidated
		r.lessThanOrEqual = &value
	}
	return r
}

func NewRequirement(key string, operator corev1.NodeSelectorOperator, values ...string) *Requirement {
	return NewRequirementWithFlexibility(key, operator, nil, values...)
}

func (r *Requirement) NodeSelectorRequirement() v1.NodeSelectorRequirementWithMinValues {
	switch {
	case r.greaterThanOrEqual != nil:
		return v1.NodeSelectorRequirementWithMinValues{
			Key:       r.Key,
			Operator:  corev1.NodeSelectorOperator(v1.NodeSelectorOpGte),
			Values:    []string{strconv.FormatInt(int64(lo.FromPtr(r.greaterThanOrEqual)), 10)},
			MinValues: r.MinValues,
		}
	case r.lessThanOrEqual != nil:
		return v1.NodeSelectorRequirementWithMinValues{
			Key:       r.Key,
			Operator:  corev1.NodeSelectorOperator(v1.NodeSelectorOpLte),
			Values:    []string{strconv.FormatInt(int64(lo.FromPtr(r.lessThanOrEqual)), 10)},
			MinValues: r.MinValues,
		}
	case r.greaterThan != nil:
		return v1.NodeSelectorRequirementWithMinValues{
			Key:       r.Key,
			Operator:  corev1.NodeSelectorOpGt,
			Values:    []string{strconv.FormatInt(int64(lo.FromPtr(r.greaterThan)), 10)},
			MinValues: r.MinValues,
		}
	case r.lessThan != nil:
		return v1.NodeSelectorRequirementWithMinValues{
			Key:       r.Key,
			Operator:  corev1.NodeSelectorOpLt,
			Values:    []string{strconv.FormatInt(int64(lo.FromPtr(r.lessThan)), 10)},
			MinValues: r.MinValues,
		}
	case r.complement:
		if len(r.values) > 0 {
			return v1.NodeSelectorRequirementWithMinValues{
				Key:       r.Key,
				Operator:  corev1.NodeSelectorOpNotIn,
				Values:    sets.List(r.values),
				MinValues: r.MinValues,
			}
		}
		return v1.NodeSelectorRequirementWithMinValues{
			Key:       r.Key,
			Operator:  corev1.NodeSelectorOpExists,
			MinValues: r.MinValues,
		}
	default:
		if len(r.values) > 0 {
			return v1.NodeSelectorRequirementWithMinValues{
				Key:       r.Key,
				Operator:  corev1.NodeSelectorOpIn,
				Values:    sets.List(r.values),
				MinValues: r.MinValues,
			}
		}
		return v1.NodeSelectorRequirementWithMinValues{
			Key:       r.Key,
			Operator:  corev1.NodeSelectorOpDoesNotExist,
			MinValues: r.MinValues,
		}
	}
}

// Intersection constraints the Requirement from the incoming requirements
// nolint:gocyclo
func (r *Requirement) Intersection(requirement *Requirement) *Requirement {
	// Complement
	complement := r.complement && requirement.complement

	// Boundaries - merge exclusive and inclusive bounds separately
	greaterThan := maxIntPtr(r.greaterThan, requirement.greaterThan)
	greaterThanOrEqual := maxIntPtr(r.greaterThanOrEqual, requirement.greaterThanOrEqual)
	lessThan := minIntPtr(r.lessThan, requirement.lessThan)
	lessThanOrEqual := minIntPtr(r.lessThanOrEqual, requirement.lessThanOrEqual)
	minValues := maxIntPtr(r.MinValues, requirement.MinValues)

	// Collapse to inclusive bounds (Gte/Lte) when both forms present
	greaterThan, greaterThanOrEqual = collapseLowerBounds(greaterThan, greaterThanOrEqual)
	lessThan, lessThanOrEqual = collapseUpperBounds(lessThan, lessThanOrEqual)

	// Check if bounds are incompatible
	if !boundsCompatible(greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual) {
		return NewRequirementWithFlexibility(r.Key, corev1.NodeSelectorOpDoesNotExist, minValues)
	}

	// Values
	var values sets.Set[string]
	if r.complement && requirement.complement {
		values = r.values.Union(requirement.values)
	} else if r.complement && !requirement.complement {
		values = requirement.values.Difference(r.values)
	} else if !r.complement && requirement.complement {
		values = r.values.Difference(requirement.values)
	} else {
		values = r.values.Intersection(requirement.values)
	}
	for value := range values {
		if !withinBounds(value, greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual) {
			values.Delete(value)
		}
	}
	// Remove boundaries for concrete sets
	if !complement {
		greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual = nil, nil, nil, nil
	}
	return &Requirement{Key: r.Key, values: values, complement: complement, greaterThan: greaterThan, greaterThanOrEqual: greaterThanOrEqual, lessThan: lessThan, lessThanOrEqual: lessThanOrEqual, MinValues: minValues}
}

// nolint:gocyclo
// HasIntersection is a more efficient implementation of Intersection
// It validates whether there is an intersection between the two requirements without actually creating the sets
// This prevents the garbage collector from having to spend cycles cleaning up all of these created set objects
func (r *Requirement) HasIntersection(requirement *Requirement) bool {
	greaterThan := maxIntPtr(r.greaterThan, requirement.greaterThan)
	greaterThanOrEqual := maxIntPtr(r.greaterThanOrEqual, requirement.greaterThanOrEqual)
	lessThan := minIntPtr(r.lessThan, requirement.lessThan)
	lessThanOrEqual := minIntPtr(r.lessThanOrEqual, requirement.lessThanOrEqual)
	if !boundsCompatible(greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual) {
		return false
	}
	// Both requirements have a complement
	if r.complement && requirement.complement {
		return true
	}
	// Only one requirement has a complement
	if r.complement && !requirement.complement {
		for value := range requirement.values {
			if !r.values.Has(value) && withinBounds(value, greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual) {
				return true
			}
		}
		return false
	}
	if !r.complement && requirement.complement {
		for value := range r.values {
			if !requirement.values.Has(value) && withinBounds(value, greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual) {
				return true
			}
		}
		return false
	}
	// Both requirements are non-complement requirements
	for value := range r.values {
		if requirement.values.Has(value) && withinBounds(value, greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual) {
			return true
		}
	}
	return false
}

func (r *Requirement) Any() string {
	switch r.Operator() {
	case corev1.NodeSelectorOpIn:
		return r.values.UnsortedList()[0]
	case corev1.NodeSelectorOpNotIn, corev1.NodeSelectorOpExists:
		min := 0
		max := math.MaxInt64
		if r.greaterThan != nil {
			min = *r.greaterThan + 1
		}
		if r.greaterThanOrEqual != nil && *r.greaterThanOrEqual > min {
			min = *r.greaterThanOrEqual
		}
		if r.lessThan != nil {
			max = *r.lessThan
		}
		if r.lessThanOrEqual != nil && *r.lessThanOrEqual+1 < max {
			max = *r.lessThanOrEqual + 1
		}
		return fmt.Sprint(rand.Intn(max-min) + min) //nolint:gosec
	}
	return ""
}

// Has returns true if the requirement allows the value
func (r *Requirement) Has(value string) bool {
	if r.complement {
		return !r.values.Has(value) && withinBounds(value, r.greaterThan, r.greaterThanOrEqual, r.lessThan, r.lessThanOrEqual)
	}
	return r.values.Has(value) && withinBounds(value, r.greaterThan, r.greaterThanOrEqual, r.lessThan, r.lessThanOrEqual)
}

func (r *Requirement) Values() []string {
	return r.values.UnsortedList()
}

func (r *Requirement) Insert(items ...string) {
	r.values.Insert(items...)
}

func (r *Requirement) Operator() corev1.NodeSelectorOperator {
	if r.complement {
		if r.Len() < math.MaxInt64 {
			return corev1.NodeSelectorOpNotIn
		}
		return corev1.NodeSelectorOpExists // corev1.NodeSelectorOpGt and corev1.NodeSelectorOpLt are treated as "Exists" with bounds
	}
	if r.Len() > 0 {
		return corev1.NodeSelectorOpIn
	}
	return corev1.NodeSelectorOpDoesNotExist
}

func (r *Requirement) Len() int {
	if r.complement {
		return math.MaxInt64 - r.values.Len()
	}
	return r.values.Len()
}

func (r *Requirement) String() string {
	var s string
	switch r.Operator() {
	case corev1.NodeSelectorOpExists, corev1.NodeSelectorOpDoesNotExist:
		s = fmt.Sprintf("%s %s", r.Key, r.Operator())
	default:
		values := sets.List(r.values)
		if length := len(values); length > 5 {
			values = append(values[:5], fmt.Sprintf("and %d others", length-5))
		}
		s = fmt.Sprintf("%s %s %s", r.Key, r.Operator(), values)
	}
	if r.greaterThan != nil {
		s += fmt.Sprintf(" >%d", *r.greaterThan)
	}
	if r.greaterThanOrEqual != nil {
		s += fmt.Sprintf(" >=%d", *r.greaterThanOrEqual)
	}
	if r.lessThan != nil {
		s += fmt.Sprintf(" <%d", *r.lessThan)
	}
	if r.lessThanOrEqual != nil {
		s += fmt.Sprintf(" <=%d", *r.lessThanOrEqual)
	}
	if r.MinValues != nil {
		s += fmt.Sprintf(" minValues %d", *r.MinValues)
	}
	return s
}

func withinBounds(valueAsString string, greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual *int) bool {
	if greaterThan == nil && greaterThanOrEqual == nil && lessThan == nil && lessThanOrEqual == nil {
		return true
	}
	value, err := strconv.Atoi(valueAsString)
	if err != nil {
		return false
	}
	// Check lower bound: Gt N means > N, Gte N means >= N
	if greaterThan != nil && value <= *greaterThan {
		return false
	}
	if greaterThanOrEqual != nil && value < *greaterThanOrEqual {
		return false
	}
	// Check upper bound: Lt N means < N, Lte N means <= N
	if lessThan != nil && value >= *lessThan {
		return false
	}
	if lessThanOrEqual != nil && value > *lessThanOrEqual {
		return false
	}
	return true
}

// boundsCompatible checks if the bounds allow any valid values
func boundsCompatible(greaterThan, greaterThanOrEqual, lessThan, lessThanOrEqual *int) bool {
	// Compute effective lower bound (inclusive): Gt N → N+1, Gte N → N
	lower := math.MinInt
	if greaterThan != nil {
		lower = *greaterThan + 1
	}
	if greaterThanOrEqual != nil && *greaterThanOrEqual > lower {
		lower = *greaterThanOrEqual
	}
	// Compute effective upper bound (inclusive): Lt N → N-1, Lte N → N
	upper := math.MaxInt
	if lessThan != nil {
		upper = *lessThan - 1
	}
	if lessThanOrEqual != nil && *lessThanOrEqual < upper {
		upper = *lessThanOrEqual
	}
	return lower <= upper
}

func minIntPtr(a, b *int) *int {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *a < *b {
		return a
	}
	return b
}

func maxIntPtr(a, b *int) *int {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *a > *b {
		return a
	}
	return b
}

// collapseLowerBounds normalizes to Gte (inclusive) form when both bounds are present.
// For integers: Gt N is equivalent to Gte N+1.
func collapseLowerBounds(greaterThan, greaterThanOrEqual *int) (*int, *int) {
	if greaterThan == nil || greaterThanOrEqual == nil {
		return greaterThan, greaterThanOrEqual
	}
	gtAsGte := *greaterThan + 1 // Gt N → Gte N+1
	if gtAsGte > *greaterThanOrEqual {
		return nil, &gtAsGte
	}
	return nil, greaterThanOrEqual
}

// collapseUpperBounds normalizes to Lte (inclusive) form when both bounds are present.
// For integers: Lt N is equivalent to Lte N-1.
func collapseUpperBounds(lessThan, lessThanOrEqual *int) (*int, *int) {
	if lessThan == nil || lessThanOrEqual == nil {
		return lessThan, lessThanOrEqual
	}
	ltAsLte := *lessThan - 1 // Lt N → Lte N-1
	if ltAsLte < *lessThanOrEqual {
		return nil, &ltAsLte
	}
	return nil, lessThanOrEqual
}
