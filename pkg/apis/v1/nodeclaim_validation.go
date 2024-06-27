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

package v1

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"
)

var (
	SupportedNodeSelectorOps = sets.NewString(
		string(v1.NodeSelectorOpIn),
		string(v1.NodeSelectorOpNotIn),
		string(v1.NodeSelectorOpGt),
		string(v1.NodeSelectorOpLt),
		string(v1.NodeSelectorOpExists),
		string(v1.NodeSelectorOpDoesNotExist),
	)

	SupportedReservedResources = sets.NewString(
		v1.ResourceCPU.String(),
		v1.ResourceMemory.String(),
		v1.ResourceEphemeralStorage.String(),
		"pid",
	)

	SupportedEvictionSignals = sets.NewString(
		"memory.available",
		"nodefs.available",
		"nodefs.inodesFree",
		"imagefs.available",
		"imagefs.inodesFree",
		"pid.available",
	)
)

func (in *NodeClaim) SupportedVerbs() []admissionregistrationv1.OperationType {
	return []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
	}
}

// Validate the NodeClaim
func (in *NodeClaim) Validate(_ context.Context) (errs *apis.FieldError) {
	return errs.Also(
		apis.ValidateObjectMetadata(in).ViaField("metadata"),
		in.Spec.validate().ViaField("spec"),
	)
}

func (in *NodeClaimSpec) validate() (errs *apis.FieldError) {
	return errs.Also(
		in.validateTaints(),
		in.validateRequirements(),
		in.Kubelet.validate().ViaField("kubeletConfiguration"),
	)
}

type taintKeyEffect struct {
	OwnerKey string
	Effect   v1.TaintEffect
}

func (in *NodeClaimSpec) validateTaints() (errs *apis.FieldError) {
	existing := map[taintKeyEffect]struct{}{}
	errs = errs.Also(in.validateTaintsField(in.Taints, existing, "taints"))
	errs = errs.Also(in.validateTaintsField(in.StartupTaints, existing, "startupTaints"))
	return errs
}

func (in *NodeClaimSpec) validateTaintsField(taints []v1.Taint, existing map[taintKeyEffect]struct{}, fieldName string) *apis.FieldError {
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

func (in *KubeletConfiguration) validate() (errs *apis.FieldError) {
	if in == nil {
		return
	}
	return errs.Also(
		validateEvictionThresholds(in.EvictionHard, "evictionHard"),
		validateEvictionThresholds(in.EvictionSoft, "evictionSoft"),
		validateReservedResources(in.KubeReserved, "kubeReserved"),
		validateReservedResources(in.SystemReserved, "systemReserved"),
		in.validateImageGCHighThresholdPercent(),
		in.validateImageGCLowThresholdPercent(),
		in.validateEvictionSoftGracePeriod(),
		in.validateEvictionSoftPairs(),
	)
}

func (in *KubeletConfiguration) validateEvictionSoftGracePeriod() (errs *apis.FieldError) {
	for k := range in.EvictionSoftGracePeriod {
		if !SupportedEvictionSignals.Has(k) {
			errs = errs.Also(apis.ErrInvalidKeyName(k, "evictionSoftGracePeriod"))
		}
	}
	return errs
}

func (in *KubeletConfiguration) validateEvictionSoftPairs() (errs *apis.FieldError) {
	evictionSoftKeys := sets.New(lo.Keys(in.EvictionSoft)...)
	evictionSoftGracePeriodKeys := sets.New(lo.Keys(in.EvictionSoftGracePeriod)...)

	evictionSoftDiff := evictionSoftKeys.Difference(evictionSoftGracePeriodKeys)
	for k := range evictionSoftDiff {
		errs = errs.Also(apis.ErrInvalidKeyName(k, "evictionSoft", "OwnerKey does not have a matching evictionSoftGracePeriod"))
	}
	evictionSoftGracePeriodDiff := evictionSoftGracePeriodKeys.Difference(evictionSoftKeys)
	for k := range evictionSoftGracePeriodDiff {
		errs = errs.Also(apis.ErrInvalidKeyName(k, "evictionSoftGracePeriod", "OwnerKey does not have a matching evictionSoft threshold value"))
	}
	return errs
}

func validateReservedResources(m map[string]string, fieldName string) (errs *apis.FieldError) {
	for k, v := range m {
		if !SupportedReservedResources.Has(k) {
			errs = errs.Also(apis.ErrInvalidKeyName(k, fieldName))
		}
		quantity, err := resource.ParseQuantity(v)
		if err != nil {
			errs = errs.Also(apis.ErrInvalidValue(v, fmt.Sprintf(`%s["%s"]`, fieldName, k), "Value must be a quantity value"))
		}
		if quantity.Value() < 0 {
			errs = errs.Also(apis.ErrInvalidValue(v, fmt.Sprintf(`%s["%s"]`, fieldName, k), "Value cannot be a negative resource quantity"))
		}
	}
	return errs
}

func validateEvictionThresholds(m map[string]string, fieldName string) (errs *apis.FieldError) {
	if m == nil {
		return
	}
	for k, v := range m {
		if !SupportedEvictionSignals.Has(k) {
			errs = errs.Also(apis.ErrInvalidKeyName(k, fieldName))
		}
		if strings.HasSuffix(v, "%") {
			p, err := strconv.ParseFloat(strings.Trim(v, "%"), 64)
			if err != nil {
				errs = errs.Also(apis.ErrInvalidValue(v, fmt.Sprintf(`%s["%s"]`, fieldName, k), fmt.Sprintf("Value could not be parsed as a percentage value, %v", err.Error())))
			}
			if p < 0 {
				errs = errs.Also(apis.ErrInvalidValue(v, fmt.Sprintf(`%s["%s"]`, fieldName, k), "Percentage values cannot be negative"))
			}
			if p > 100 {
				errs = errs.Also(apis.ErrInvalidValue(v, fmt.Sprintf(`%s["%s"]`, fieldName, k), "Percentage values cannot be greater than 100"))
			}
		} else {
			_, err := resource.ParseQuantity(v)
			if err != nil {
				errs = errs.Also(apis.ErrInvalidValue(v, fmt.Sprintf("%s[%s]", fieldName, k), fmt.Sprintf("Value could not be parsed as a resource quantity, %v", err.Error())))
			}
		}
	}
	return errs
}

// Validate validateImageGCHighThresholdPercent
func (in *KubeletConfiguration) validateImageGCHighThresholdPercent() (errs *apis.FieldError) {
	if in.ImageGCHighThresholdPercent != nil && ptr.Int32Value(in.ImageGCHighThresholdPercent) < ptr.Int32Value(in.ImageGCLowThresholdPercent) {
		return errs.Also(apis.ErrInvalidValue("must be greater than imageGCLowThresholdPercent", "imageGCHighThresholdPercent"))
	}

	return errs
}

// Validate imageGCLowThresholdPercent
func (in *KubeletConfiguration) validateImageGCLowThresholdPercent() (errs *apis.FieldError) {
	if in.ImageGCHighThresholdPercent != nil && ptr.Int32Value(in.ImageGCLowThresholdPercent) > ptr.Int32Value(in.ImageGCHighThresholdPercent) {
		return errs.Also(apis.ErrInvalidValue("must be less than imageGCHighThresholdPercent", "imageGCLowThresholdPercent"))
	}

	return errs
}
