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

	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/util/validation"
)

// RuntimeValidate will be used to validate any part of the CRD that can not be validated at CRD creation
func (in *NodePool) RuntimeValidate(ctx context.Context) (errs error) {
	errs = multierr.Combine(in.Spec.Template.validateLabels(), in.Spec.Template.Spec.validateTaints(), in.Spec.Template.Spec.validateRequirements(ctx), in.Spec.Template.validateRequirementsNodePoolKeyDoesNotExist(), in.validateConsolidationFields())
	return errs
}

// validateConsolidationFields validates that consolidateWhen and consolidationPolicy are not set to conflicting values.
// When consolidateWhen is set, it takes precedence over consolidationPolicy.
func (in *NodePool) validateConsolidationFields() (errs error) {
	// Warn if consolidationPolicy is WhenEmpty but consolidateWhen is set to something that allows non-empty consolidation
	if in.Spec.Disruption.ConsolidationPolicy == ConsolidationPolicyWhenEmpty &&
		in.Spec.Disruption.ConsolidateWhen != "" &&
		in.Spec.Disruption.ConsolidateWhen != ConsolidateWhenEmpty {
		errs = multierr.Append(errs, fmt.Errorf("consolidateWhen %q conflicts with consolidationPolicy %q; consolidateWhen takes precedence when set",
			in.Spec.Disruption.ConsolidateWhen, in.Spec.Disruption.ConsolidationPolicy))
	}
	// Validate decisionRatioThreshold is only meaningful with WhenCostJustifiesDisruption
	if in.Spec.Disruption.DecisionRatioThreshold != nil &&
		in.Spec.Disruption.ConsolidateWhen != ConsolidateWhenCostJustifiesDisruption {
		// Not an error, but the threshold will be ignored — this is informational
	}
	return errs
}

func (in *NodeClaimTemplate) validateLabels() (errs error) {
	for key, value := range in.Labels {
		if key == NodePoolLabelKey {
			errs = multierr.Append(errs, fmt.Errorf("invalid key name %q in labels, restricted", key))
		}
		for _, err := range validation.IsQualifiedName(key) {
			errs = multierr.Append(errs, fmt.Errorf("invalid key name %q in labels, %q", key, err))
		}
		for _, err := range validation.IsValidLabelValue(value) {
			errs = multierr.Append(errs, fmt.Errorf("invalid value: %s for label[%s], %s", value, key, err))
		}
		if err := IsRestrictedLabel(key); err != nil {
			errs = multierr.Append(errs, fmt.Errorf("invalid key name %q in labels, %s", key, err.Error()))
		}
	}
	return errs
}

func (in *NodeClaimTemplate) validateRequirementsNodePoolKeyDoesNotExist() (errs error) {
	for _, requirement := range in.Spec.Requirements {
		if requirement.Key == NodePoolLabelKey {
			errs = multierr.Append(errs, fmt.Errorf("invalid key: %q in requirements, restricted", requirement.Key))
		}
	}
	return errs
}
