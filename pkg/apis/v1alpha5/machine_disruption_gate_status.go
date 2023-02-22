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

package v1alpha5

import (
	"knative.dev/pkg/apis"
)

// MachineDisruptionGateStatus defines the observed state of MachineDisruptionGate
type MachineDisruptionGateStatus struct {
	// NextActiveTime is the next time the MachineDisruptionGate will be active.
	// Will return the current time if it is currently active.
	// +optional
	// +kubebuilder:validation:Format="date-time"
	NextActiveTime *apis.VolatileTime `json:"nextActiveTime,omitempty"`
	// Conditions contains signals for health and readiness
	// +optional
	Conditions apis.Conditions `json:"conditions,omitempty"`
}

func (m *MachineDisruptionGate) StatusConditions() apis.ConditionManager {
	return apis.NewLivingConditionSet(
		Active,
	).Manage(m)
}

func (m *MachineDisruptionGate) GetConditions() apis.Conditions {
	return m.Status.Conditions
}

func (m *MachineDisruptionGate) SetConditions(conditions apis.Conditions) {
	m.Status.Conditions = conditions
}
