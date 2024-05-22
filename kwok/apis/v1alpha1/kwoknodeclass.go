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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KwokNodeClassSpec is the top level specification for the Kwok Provider.
// This is a dummy node class for Kwok Karpenter installations so that we can have a readiness condition on NodePools.
type KwokNodeClassSpec struct {
}

// KwokNodeClass is the Schema for the KwokNodeClass API
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=kwoknodeclasses,scope=Cluster,categories=karpenter,shortName={kwoknc,kwokncs}
// +kubebuilder:subresource:status
type KwokNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec KwokNodeClassSpec `json:"spec,omitempty"`
	// +kubebuilder:default:={conditions: {{type: "Ready", status: "True", reason:"NodeClassReady", lastTransitionTime: "2024-01-01T01:01:01Z", message: ""}}}
	Status KwokNodeClassStatus `json:"status,omitempty"`
}

// KwokNodeClassList contains a list of KwokNodeClass
// +kubebuilder:object:root=true
type KwokNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KwokNodeClass `json:"items"`
}
