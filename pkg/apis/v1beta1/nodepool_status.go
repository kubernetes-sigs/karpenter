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

package v1beta1

import (
	v1 "k8s.io/api/core/v1"
)

// NodePoolStatus defines the observed state of NodePool
type NodePoolStatus struct {
	// Resources is the list of resources that have been provisioned.
	// +optional
	Resources v1.ResourceList `json:"resources,omitempty"`
	// Conditions represent the latest available observations of a NodePool's current state.
	// +optional
	Conditions []NodePoolCondition `json:"conditions,omitempty"`
}

// NodePoolConditionType represents a NodePool condition value.
type NodePoolCondition struct {
	
	// Type of NodePool condition.
	Type NodePoolConditionType `json:"type"`
	// Status of the condition, one of True, False, Unknown.
	Status v1.ConditionStatus `json:"status"`

}

// NodePoolConditionType represents a NodePool condition value.
type NodePoolConditionType string

const (
	// NodeClassConditionTypeReady represents a NodePool condition is in ready state.
	NodeClassConditionTypeReady NodePoolConditionType = "Ready"
)	


func (np *NodePool) GetCondition(conditionType NodePoolConditionType) bool {
	for _, c := range np.Status.Conditions {
		if c.Type == conditionType {
			return c.Status == v1.ConditionStatus(v1.ConditionTrue)
		}
	}
	return false
}