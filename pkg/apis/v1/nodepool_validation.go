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
	"errors"
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (in *NodePool) SupportedVerbs() []admissionregistrationv1.OperationType {
	return []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
	}
}

// RuntimeValidate will be used to validate any part of the CRD that can not be validated at CRD creation
func (in *NodePool) RuntimeValidate() error {
	return errors.Join(
		in.Spec.Template.validateLabels(),
		in.Spec.Template.Spec.validateTaints(),
		in.Spec.Template.Spec.validateRequirements(),
		in.Spec.Template.validateRequirementsNodePoolKeyDoesNotExist(),
		ValidateObjectMetadata(in),
	)
}

func (in *NodeClaimTemplate) validateLabels() error {
	var errs error
	for key, value := range in.Labels {
		if key == NodePoolLabelKey {
			errs = errors.Join(errs, fmt.Errorf("invalid label key name %s, key is restricted", key))
		}
		for _, err := range validation.IsQualifiedName(key) {
			errs = errors.Join(errs, fmt.Errorf("invalid label key name %s, %v", key, err))
		}
		for _, err := range validation.IsValidLabelValue(value) {
			errs = errors.Join(errs, fmt.Errorf("invalid label value %s, for key name %s, %v", value, key, err))
		}
		if err := IsRestrictedLabel(key); err != nil {
			errs = errors.Join(errs, fmt.Errorf("invalid label key name %s, %w", key, err))
		}
	}
	return errs
}

func (in *NodeClaimTemplate) validateRequirementsNodePoolKeyDoesNotExist() error {
	var errs error
	for _, requirement := range in.Spec.Requirements {
		if requirement.Key == NodePoolLabelKey {
			errs = errors.Join(errs, fmt.Errorf("invalid requirement key: %s", requirement.Key))
		}
	}
	return errs
}

func ValidateObjectMetadata(meta metav1.Object) error {
	name := meta.GetName()
	generateName := meta.GetGenerateName()

	if generateName != "" {
		msgs := apivalidation.NameIsDNS1035Label(generateName, true)

		if len(msgs) > 0 {
			return fmt.Errorf("not a DNS 1035 label prefix: %v", msgs)
		}
	}

	if name != "" {
		msgs := apivalidation.NameIsDNS1035Label(name, false)

		if len(msgs) > 0 {
			return fmt.Errorf("not a DNS 1035 label prefix: %v", msgs)
		}
	}

	if generateName == "" && name == "" {
		return fmt.Errorf("name or generateName is required")
	}

	return nil
}
