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

	"k8s.io/apimachinery/pkg/util/validation"
	"knative.dev/pkg/apis"
)

// RuntimeValidate will be used to validate any part of the CRD that can not be validated at CRD creation
func (in *NodePool) RuntimeValidate() (errs *apis.FieldError) {
	return errs.Also(
		in.Spec.Template.validateLabels().ViaField("spec.template.metadata"),
		in.Spec.Template.Spec.validateTaints().ViaField("spec.template.spec"),
		in.Spec.Template.Spec.validateRequirements().ViaField("spec.template.spec"),
		in.Spec.Template.validateRequirementsNodePoolKeyDoesNotExist().ViaField("spec.template.spec"),
	)
}

func (in *NodeClaimTemplate) validateLabels() (errs *apis.FieldError) {
	for key, value := range in.Labels {
		if key == NodePoolLabelKey {
			errs = errs.Also(apis.ErrInvalidKeyName(key, "labels", "restricted"))
		}
		for _, err := range validation.IsQualifiedName(key) {
			errs = errs.Also(apis.ErrInvalidKeyName(key, "labels", err))
		}
		for _, err := range validation.IsValidLabelValue(value) {
			errs = errs.Also(apis.ErrInvalidValue(fmt.Sprintf("%s, %s", value, err), fmt.Sprintf("labels[%s]", key)))
		}
		if err := IsRestrictedLabel(key); err != nil {
			errs = errs.Also(apis.ErrInvalidKeyName(key, "labels", err.Error()))
		}
	}
	return errs
}

func (in *NodeClaimTemplate) validateRequirementsNodePoolKeyDoesNotExist() (errs *apis.FieldError) {
	for i, requirement := range in.Spec.Requirements {
		if requirement.Key == NodePoolLabelKey {
			errs = errs.Also(apis.ErrInvalidArrayValue(fmt.Sprintf("%s is restricted", requirement.Key), "requirements", i))
		}
	}
	return errs
}
