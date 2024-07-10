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
	"strconv"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"knative.dev/pkg/apis"
)

var SupportedNodeSelectorOps = sets.NewString(
	string(v1.NodeSelectorOpIn),
	string(v1.NodeSelectorOpNotIn),
	string(v1.NodeSelectorOpGt),
	string(v1.NodeSelectorOpLt),
	string(v1.NodeSelectorOpExists),
	string(v1.NodeSelectorOpDoesNotExist),
)

type taintKeyEffect struct {
	OwnerKey string
	Effect   v1.TaintEffect
}

func (in *NodeClaimSpec) validateTaints() (errs *apis.FieldError) {
	existing := map[taintKeyEffect]struct{}{}
	errs = errs.Also(validateTaintsField(in.Taints, existing, "taints"))
	errs = errs.Also(validateTaintsField(in.StartupTaints, existing, "startupTaints"))
	return errs
}

func validateTaintsField(taints []v1.Taint, existing map[taintKeyEffect]struct{}, fieldName string) *apis.FieldError {
	var errs *apis.FieldError
	for i, taint := range taints {
		// Validate OwnerKey
		if len(taint.Key) == 0 {
			errs = errs.Also(apis.ErrInvalidArrayValue(errs, fieldName, i))
		}
		for _, err := range validation.IsQualifiedName(taint.Key) {
			errs = errs.Also(apis.ErrInvalidArrayValue(err, fieldName, i))
		}
		// Validate Value
		if len(taint.Value) != 0 {
			for _, err := range validation.IsQualifiedName(taint.Value) {
				errs = errs.Also(apis.ErrInvalidArrayValue(err, fieldName, i))
			}
		}
		// Validate effect
		switch taint.Effect {
		case v1.TaintEffectNoSchedule, v1.TaintEffectPreferNoSchedule, v1.TaintEffectNoExecute, "":
		default:
			errs = errs.Also(apis.ErrInvalidArrayValue(taint.Effect, fieldName, i))
		}

		// Check for duplicate OwnerKey/Effect pairs
		key := taintKeyEffect{OwnerKey: taint.Key, Effect: taint.Effect}
		if _, ok := existing[key]; ok {
			errs = errs.Also(apis.ErrGeneric(fmt.Sprintf("duplicate taint Key/Effect pair %s=%s", taint.Key, taint.Effect), apis.CurrentField).
				ViaFieldIndex("taints", i))
		}
		existing[key] = struct{}{}
	}
	return errs
}

// This function is used by the NodeClaim validation webhook to verify the nodepool requirements.
// When this function is called, the nodepool's requirements do not include the requirements from labels.
// NodeClaim requirements only support well known labels.
func (in *NodeClaimSpec) validateRequirements() (errs *apis.FieldError) {
	for i, requirement := range in.Requirements {
		if err := ValidateRequirement(requirement); err != nil {
			errs = errs.Also(apis.ErrInvalidArrayValue(err, "requirements", i))
		}
	}
	return errs
}

func ValidateRequirement(requirement NodeSelectorRequirementWithMinValues) error { //nolint:gocyclo
	var errs error
	if normalized, ok := NormalizedLabels[requirement.Key]; ok {
		requirement.Key = normalized
	}
	if !SupportedNodeSelectorOps.Has(string(requirement.Operator)) {
		errs = multierr.Append(errs, fmt.Errorf("key %s has an unsupported operator %s not in %s", requirement.Key, requirement.Operator, SupportedNodeSelectorOps.UnsortedList()))
	}
	if e := IsRestrictedLabel(requirement.Key); e != nil {
		errs = multierr.Append(errs, e)
	}
	for _, err := range validation.IsQualifiedName(requirement.Key) {
		errs = multierr.Append(errs, fmt.Errorf("key %s is not a qualified name, %s", requirement.Key, err))
	}
	for _, value := range requirement.Values {
		for _, err := range validation.IsValidLabelValue(value) {
			errs = multierr.Append(errs, fmt.Errorf("invalid value %s for key %s, %s", value, requirement.Key, err))
		}
	}
	if requirement.Operator == v1.NodeSelectorOpIn && len(requirement.Values) == 0 {
		errs = multierr.Append(errs, fmt.Errorf("key %s with operator %s must have a value defined", requirement.Key, requirement.Operator))
	}

	if requirement.Operator == v1.NodeSelectorOpIn && requirement.MinValues != nil && len(requirement.Values) < lo.FromPtr(requirement.MinValues) {
		errs = multierr.Append(errs, fmt.Errorf("key %s with operator %s must have at least minimum number of values defined in 'values' field", requirement.Key, requirement.Operator))
	}

	if requirement.Operator == v1.NodeSelectorOpGt || requirement.Operator == v1.NodeSelectorOpLt {
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
