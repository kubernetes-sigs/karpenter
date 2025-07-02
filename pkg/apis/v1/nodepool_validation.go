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
	errs = multierr.Combine(
		in.Spec.Template.validateLabels(),
		in.Spec.Template.Spec.validateTaints(),
		in.Spec.Template.Spec.validateRequirements(ctx),
		in.Spec.Template.validateRequirementsNodePoolKeyDoesNotExist(),
		in.validateReplicasConstraints(),
	)
	return errs
}

// validateReplicasConstraints validates that when replicas is set, incompatible fields are not set
func (in *NodePool) validateReplicasConstraints() (errs error) {
	if in.Spec.Replicas == nil {
		return nil
	}

	if in.Spec.Weight != nil {
		errs = multierr.Append(errs, fmt.Errorf("weight cannot be specified when replicas is set"))
	}

	// We add kubebuilder defaults to consolidationPolicy, anything other than default is rejected
	if in.Spec.Disruption.ConsolidationPolicy != ConsolidationPolicyWhenEmptyOrUnderutilized {
		errs = multierr.Append(errs, fmt.Errorf("disruption.consolidationPolicy cannot be specified when replicas is set"))
	}

	// We add kubebuilder defaults to consolidateAfter, anything other than default is rejected
	consolidateAfter := in.Spec.Disruption.ConsolidateAfter
	if consolidateAfter.Duration != nil || len(consolidateAfter.Raw) > 0 {
		errs = multierr.Append(errs, fmt.Errorf("disruption.consolidateAfter cannot be specified when replicas is set"))
	}

	if in.Spec.Limits != nil {
		errs = multierr.Append(errs, fmt.Errorf("limits cannot be specified when replicas is set"))
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
