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
	"sort"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type NodeOverlaySpec struct {
	// Requirements are layered with GetLabels and applied to every node.
	// +kubebuilder:validation:XValidation:message="requirements with operator 'In' must have a value defined",rule="self.all(x, x.operator == 'In' ? x.values.size() != 0 : true)"
	// +kubebuilder:validation:XValidation:message="requirements operator 'Gt' or 'Lt' must have a single positive integer value",rule="self.all(x, (x.operator == 'Gt' || x.operator == 'Lt') ? (x.values.size() == 1 && int(x.values[0]) >= 0) : true)"
	// +kubebuilder:validation:MaxItems:=100
	// +required
	Requirements []v1.NodeSelectorRequirement `json:"requirements,omitempty"`
	// UPDATE WORDING: PricePercent modifies the price of the simulated node (PriceAdjustment + (Price * PricePercent / 100)).
	// Update Validation
	// +kubebuilder:default:="100%"
	// +optional
	PriceAdjustment string `json:"priceAdjustment,omitempty"`
	// Capacity adds extended resource to instances types based on the selector provided
	// +optional
	Capacity v1.ResourceList `json:"capacity,omitempty"`
	// Weight is the priority given to the nodeoverlay while overriding node attributes. A higher
	// numerical weight indicates that this nodeoverlay will be ordered
	// ahead of other nodeoverlay with lower weights. A nodeoverlay with no weight
	// will be treated as if it is a nodeoverlay with a weight of 0. Two nodeoverlays that have the same weight,
	// we will merge them in alphabetical order.
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=100
	// +optional
	Weight *int64 `json:"weight,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:path=nodeoverlays,scope=Cluster,categories=karpenter
type NodeOverlay struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:message="need to define a capacity or priceAdjustment field ",rule="has(self.capacity) || has(self.priceAdjustment)"
	// +required
	Spec   NodeOverlaySpec   `json:"spec"`
	Status NodeOverlayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type NodeOverlayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeOverlay `json:"items"`
}

// OrderByWeight orders the NodeOverlays in the provided slice by their priority weight in-place. This priority evaluates
// the following things in precedence order:
//  1. NodeOverlays that have a larger weight are ordered first
//  2. If two NodeOverlays have the same weight, then the NodePool with the name later in the alphabet will come first
func (nol *NodeOverlayList) OrderByWeight() {
	sort.Slice(nol.Items, func(a, b int) bool {
		weightA := lo.FromPtr(nol.Items[a].Spec.Weight)
		weightB := lo.FromPtr(nol.Items[b].Spec.Weight)
		if weightA == weightB {
			// Order NodePools by name for a consistent ordering when sorting equal weight
			return nol.Items[a].Name > nol.Items[b].Name
		}
		return weightA > weightB
	})
}
