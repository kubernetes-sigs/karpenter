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
	"strconv"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// RuntimeValidate will be used to validate any part of the CRD that can not be validated at CRD creation
func (in *NodeOverlay) RuntimeValidate(ctx context.Context) error {
	return multierr.Combine(in.Spec.validateRequirements(ctx), in.Spec.validateCapacity())
}

// This function is used by the NodeClaim validation webhook to verify the nodepool requirements.
// When this function is called, the nodepool's requirements do not include the requirements from labels.
// NodeClaim requirements only support well known labels.
func (in *NodeOverlaySpec) validateRequirements(ctx context.Context) (errs error) {
	for _, requirement := range in.Requirements {
		if err := ValidateRequirement(ctx, requirement); err != nil {
			errs = multierr.Append(errs, fmt.Errorf("invalid value: %w in requirements, restricted", err))
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

func ValidateRequirement(ctx context.Context, requirement corev1.NodeSelectorRequirement) error { //nolint:gocyclo
	var errs error
	if normalized, ok := v1.NormalizedLabels[requirement.Key]; ok {
		requirement.Key = normalized
	}
	if !v1.SupportedNodeSelectorOps.Has(string(requirement.Operator)) {
		errs = multierr.Append(errs, fmt.Errorf("key %s has an unsupported operator %s not in %s", requirement.Key, requirement.Operator, v1.SupportedNodeSelectorOps.UnsortedList()))
	}
	if e := v1.IsRestrictedLabel(requirement.Key); e != nil {
		errs = multierr.Append(errs, e)
	}
	// Validate that at least one value is valid for well-known labels with known values
	if err := validateWellKnownValues(ctx, requirement); err != nil {
		errs = multierr.Append(errs, err)
	}
	for _, err := range validation.IsQualifiedName(requirement.Key) {
		errs = multierr.Append(errs, fmt.Errorf("key %s is not a qualified name, %s", requirement.Key, err))
	}
	for _, value := range requirement.Values {
		for _, err := range validation.IsValidLabelValue(value) {
			errs = multierr.Append(errs, fmt.Errorf("invalid value %s for key %s, %s", value, requirement.Key, err))
		}
	}
	if requirement.Operator == corev1.NodeSelectorOpIn && len(requirement.Values) == 0 {
		errs = multierr.Append(errs, fmt.Errorf("key %s with operator %s must have a value defined", requirement.Key, requirement.Operator))
	}

	if requirement.Operator == corev1.NodeSelectorOpGt || requirement.Operator == corev1.NodeSelectorOpLt {
		if len(requirement.Values) != 1 {
			errs = multierr.Append(errs, fmt.Errorf("key %s with operator %s must have a single positive integer value", requirement.Key, requirement.Operator))
		} else {
			value, err := strconv.Atoi(requirement.Values[0])
			if err != nil || value < 0 {
				errs = multierr.Append(errs, fmt.Errorf("key %s with operator %s must have a single positive integer value", requirement.Key, requirement.Operator))
			}
		}
	}
	return errs
}

// ValidateWellKnownValues checks if the requirement has well known values.
// An error will cause a NodePool's Readiness to transition to False.
// It returns an error if all values are invalid.
// It returns an error if there are not enough valid values to satisfy min values for a requirement with known values.
// It logs if invalid values are found but valid values can be used.
func validateWellKnownValues(ctx context.Context, requirement corev1.NodeSelectorRequirement) error {
	// If the key doesn't have well-known values or the operator is not In, nothing to validate
	if !v1.WellKnownLabels.Has(requirement.Key) || requirement.Operator != corev1.NodeSelectorOpIn {
		return nil
	}

	// If the key doesn't have well-known values defined, nothing to validate
	knownValues, exists := v1.WellKnownValuesForRequirements[requirement.Key]
	if !exists {
		return nil
	}

	values, invalidValues := lo.FilterReject(requirement.Values, func(val string, _ int) bool {
		return knownValues.Has(val)
	})

	// If there are only invalid values, set an error to transition the nodepool's readiness to false
	if len(values) == 0 {
		return fmt.Errorf("no valid values found in %v for %s, expected one of: %v, got: %v",
			requirement.Values, requirement.Key, knownValues, invalidValues)
	}

	// If there are valid and invalid values, log the invalid values and proceed with valid values
	if len(invalidValues) > 0 {
		log.FromContext(ctx).Error(fmt.Errorf("invalid values found for key"), "please correct found invalid values, proceeding with valid values", "key", requirement.Key, "valid-values", values, "invalid-values", invalidValues)
	}

	return nil
}
