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
	"github.com/awslabs/operatorpkg/status"
)

// KwokNodeClassStatus contains the resolved state of the KwokNodeClass
type KwokNodeClassStatus struct {
	// Conditions contains signals for health and readiness
	Conditions []status.Condition `json:"conditions,omitempty"`
}

func (in *KwokNodeClass) StatusConditions() status.ConditionSet {
	return status.NewReadyConditions().For(in)
}

func (in *KwokNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *KwokNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
