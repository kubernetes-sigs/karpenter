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

package cloudprovider

import (
	"fmt"
	"slices"
	"strings"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// ValidateDynamicResources reports violations of the DynamicResources conventions that can be detected statically from
// a resolved instance type (see DynamicResources). In addition to the per-set conventions, it checks that every key a
// set's requirements reference is a well-known label, that the instance type's requirements define each such key as
// the union of its offerings' values (or DoesNotExist), and that offerings that differ only in those keys have the
// same price. Cloud providers are expected to call it from their tests; it isn't evaluated at runtime.
//
//nolint:gocyclo
func ValidateDynamicResources(it *InstanceType) []error {
	var errs []error
	setKeys := sets.New[string]()
	setsByDriver := map[string][]int{}
	seenDevices := map[DeviceID]int{}
	for idx, dr := range it.DynamicResources {
		drivers := sets.New[string]()
		for _, t := range dr.ResourceSliceTemplates {
			drivers.Insert(t.Driver.Value())
			for _, d := range t.Devices {
				id := DeviceID{Driver: t.Driver, Pool: t.Pool.Name, Device: d.Name}
				if prev, ok := seenDevices[id]; ok && prev != idx {
					errs = append(errs, fmt.Errorf("instance type %q: device %s/%s/%s is in sets %d and %d", it.Name, id.Driver.Value(), id.Pool.Value(), id.Device.Value(), prev, idx))
				}
				seenDevices[id] = idx
			}
		}
		if drivers.Len() > 1 {
			errs = append(errs, fmt.Errorf("instance type %q: set %d has templates for multiple drivers %v", it.Name, idx, sets.List(drivers)))
		}
		for driver := range drivers {
			setsByDriver[driver] = append(setsByDriver[driver], idx)
		}
		if !slices.ContainsFunc(it.Offerings, func(o *Offering) bool { return compatibleWithSet(dr, o) }) {
			errs = append(errs, fmt.Errorf("instance type %q: set %d (%s) is not compatible with any offering", it.Name, idx, dr.Requirements))
		}
		for key := range dr.Requirements {
			setKeys.Insert(key)
			if !v1.WellKnownLabels.Has(key) {
				errs = append(errs, fmt.Errorf("instance type %q: set %d references key %q, which is not a well-known label", it.Name, idx, key))
			}
		}
	}
	for driver, idxs := range setsByDriver {
		if len(idxs) < 2 {
			continue
		}
		for _, o := range it.Offerings {
			matched := lo.Filter(idxs, func(idx int, _ int) bool { return compatibleWithSet(it.DynamicResources[idx], o) })
			if len(matched) > 1 {
				errs = append(errs, fmt.Errorf("instance type %q: offering (%s) is compatible with sets %v of driver %q", it.Name, o.Requirements, matched, driver))
			}
		}
	}
	for _, key := range sets.List(setKeys) {
		errs = append(errs, validateUnionRule(it, key)...)
	}
	errs = append(errs, validateModeInvariantPrice(it, setKeys)...)
	return errs
}

func compatibleWithSet(dr *DynamicResources, o *Offering) bool {
	return dr.Requirements.IsCompatible(o.Requirements, scheduling.AllowUndefinedWellKnownLabels)
}

// validateUnionRule checks that the instance type defines key as In the union of its offerings' values for key, or as
// DoesNotExist when no offering carries a value for it.
func validateUnionRule(it *InstanceType, key string) []error {
	if !it.Requirements.Has(key) {
		return []error{fmt.Errorf("instance type %q: requirements don't define set key %q", it.Name, key)}
	}
	union := sets.New[string]()
	for _, o := range it.Offerings {
		if req, ok := o.Requirements[key]; ok && req.Operator() == corev1.NodeSelectorOpIn {
			union.Insert(req.Values()...)
		}
	}
	itReq := it.Requirements.Get(key)
	switch itReq.Operator() {
	case corev1.NodeSelectorOpDoesNotExist:
		if union.Len() != 0 {
			return []error{fmt.Errorf("instance type %q: key %q is DoesNotExist, but offerings carry %v", it.Name, key, sets.List(union))}
		}
	case corev1.NodeSelectorOpIn:
		if values := sets.New(itReq.Values()...); !values.Equal(union) {
			return []error{fmt.Errorf("instance type %q: key %q is In %v, but its offerings carry %v", it.Name, key, sets.List(values), sets.List(union))}
		}
	default:
		return []error{fmt.Errorf("instance type %q: key %q must be In or DoesNotExist, got %s", it.Name, key, itReq.Operator())}
	}
	return nil
}

// validateModeInvariantPrice checks that offerings which differ only in set keys have the same price.
func validateModeInvariantPrice(it *InstanceType, setKeys sets.Set[string]) []error {
	if setKeys.Len() == 0 {
		return nil
	}
	prices := map[string]float64{}
	var errs []error
	for _, o := range it.Offerings {
		key := requirementsKeyWithout(o.Requirements, setKeys)
		if price, ok := prices[key]; ok && price != o.Price {
			errs = append(errs, fmt.Errorf("instance type %q: offerings (%s) differ only in set keys but have prices %v and %v", it.Name, key, price, o.Price))
			continue
		}
		prices[key] = o.Price
	}
	return errs
}

// requirementsKeyWithout returns a deterministic, untruncated representation of reqs without the excluded keys.
func requirementsKeyWithout(reqs scheduling.Requirements, excluded sets.Set[string]) string {
	parts := make([]string, 0, len(reqs))
	for key, req := range reqs {
		if excluded.Has(key) {
			continue
		}
		values := req.Values()
		slices.Sort(values)
		parts = append(parts, fmt.Sprintf("%s %s %v", key, req.Operator(), values))
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}
