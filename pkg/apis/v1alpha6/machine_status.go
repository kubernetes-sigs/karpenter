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

package v1alpha6

import (
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/apis"
)

// ProvisionerStatus defines the observed state of Provisioner
type MachineStatus struct {
	// ProviderID of the corresponding node object
	ProviderID string `json:"providerID,omitempty"`
	// Allocatable is the resources available to use for scheduling
	Allocatable v1.ResourceList `json:"allocatable,omitempty"`
	// Properties of the node that may be used for scheduling
	Labels map[string]string `json:"labels,omitempty`
	// Conditions contains signals for health and readiness
	// +optional
	Conditions apis.Conditions `json:"conditions,omitempty"`
}

func (m *Machine) StatusConditions() apis.ConditionManager {
	return apis.NewLivingConditionSet(
		MachineCreated,
		MachineInitialized,
		MachineHealthy,
	).Manage(m)
}

var (
	MachineCreated     apis.ConditionType
	MachineInitialized apis.ConditionType
	MachineHealthy apis.ConditionType
)

func (m *Machine) GetConditions() apis.Conditions {
	return m.Status.Conditions
}

func (m *Machine) SetConditions(conditions apis.Conditions) {
	m.Status.Conditions = conditions
}
