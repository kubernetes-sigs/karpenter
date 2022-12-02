/*
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
	"knative.dev/pkg/apis"
)

// MachineStatus defines the observed state of Machine
type MachineStatus struct {
	// ProviderID of the corresponding node object
	ProviderID string `json:"providerID,omitempty"`
	// Conditions contains signals for health and readiness
	// +optional
	Conditions apis.Conditions `json:"conditions,omitempty"`
}

func (in *Machine) StatusConditions() apis.ConditionManager {
	return apis.NewLivingConditionSet(
		MachineCreated,
		MachineRegistered,
		MachineInitialized,
		MachineHealthy,
	).Manage(in)
}

var (
	MachineCreated     apis.ConditionType
	MachineRegistered  apis.ConditionType
	MachineInitialized apis.ConditionType
	MachineHealthy     apis.ConditionType
)

func (in *Machine) GetConditions() apis.Conditions {
	return in.Status.Conditions
}

func (in *Machine) SetConditions(conditions apis.Conditions) {
	in.Status.Conditions = conditions
}
