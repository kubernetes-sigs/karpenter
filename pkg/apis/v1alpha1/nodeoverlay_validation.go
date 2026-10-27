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

package v1alpha1

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// RuntimeValidate will be used to validate any part of the CRD that can not be validated at CRD creation
func (in *NodeOverlay) RuntimeValidate(ctx context.Context) error {
	return multierr.Combine(in.Spec.validateRequirements(ctx), in.Spec.validateCapacity(), in.Spec.validateMaxCapacity())
}

// This function is used by the NodeOverlay validation webhook to verify the nodeoverlay requirements.
// When this function is called, the nodeoverlay's requirements do not include the requirements from labels.
// NodeOverlay requirements only support well known labels.
func (in *NodeOverlaySpec) validateRequirements(ctx context.Context) (errs error) {
	for _, requirement := range in.Requirements {
		if err := v1.ValidateRequirement(ctx, v1.NodeSelectorRequirementWithMinValues{Key: requirement.Key, Operator: requirement.Operator, Values: requirement.Values}); err != nil {
			errs = multierr.Append(errs, fmt.Errorf("invalid value: %w in requirements, restricted", err))
		}
		if requirement.Operator == corev1.NodeSelectorOpNotIn && len(requirement.Values) == 0 {
			errs = multierr.Append(errs, fmt.Errorf("key %s with operator %s must have a value defined", requirement.Key, requirement.Operator))
		}
	}
	return errs
}

func (in *NodeOverlaySpec) validateCapacity() (errs error) {
	for n := range in.Capacity {
		if v1.WellKnownResources.Has(n) {
			errs = multierr.Append(errs, fmt.Errorf("invalid capacity: %s in resource, restricted", n))
		}
	}
	return errs
}

func (in *NodeOverlaySpec) validateMaxCapacity() (errs error) {
	for n, q := range in.MaxCapacity {
		if v1.WellKnownResources.Has(n) {
			errs = multierr.Append(errs, fmt.Errorf("invalid maxCapacity: %s in resource, restricted", n))
		}
		if _, ok := in.Capacity[n]; ok {
			errs = multierr.Append(errs, fmt.Errorf("invalid maxCapacity: %s is also set in capacity", n))
		}
		if q.Sign() <= 0 {
			errs = multierr.Append(errs, fmt.Errorf("invalid maxCapacity: %s must be greater than zero", n))
			continue
		}
		if err := validateHugePagesMultiple(n, q); err != nil {
			errs = multierr.Append(errs, err)
		}
	}
	return errs
}

// validateHugePagesMultiple checks that a hugepages-<size> quantity is a whole number of pages. Other resources pass.
func validateHugePagesMultiple(name corev1.ResourceName, quantity resource.Quantity) error {
	if !strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix) {
		return nil
	}
	pageSize, err := resource.ParseQuantity(strings.TrimPrefix(string(name), corev1.ResourceHugePagesPrefix))
	if err != nil {
		return fmt.Errorf("invalid maxCapacity: %s has an invalid page size, %w", name, err)
	}
	if pageSize.Value() <= 0 {
		return fmt.Errorf("invalid maxCapacity: %s has an invalid page size", name)
	}
	if quantity.Value()%pageSize.Value() != 0 {
		return fmt.Errorf("invalid maxCapacity: %s must be a whole number of %s pages", name, pageSize.String())
	}
	return nil
}
