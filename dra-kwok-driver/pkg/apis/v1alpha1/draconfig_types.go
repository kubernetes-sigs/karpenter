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
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DRAConfigSpec defines the desired state of DRAConfig
type DRAConfigSpec struct {
	//nolint:kubeapilinter
	// driver specifies the DRA driver name that will manage these resources.
	// Supports simulating multiple drivers
	//
	// +required
	// +kubebuilder:validation:Pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$
	Driver string `json:"driver"`

	// mappings defines how KWOK nodes map to ResourceSlices.
	// Each mapping specifies node selectors and the ResourceSlice configuration to create.
	// Multiple mappings allow different device configurations for different node types.
	//
	// +kubebuilder:validation:MinItems=1
	// +required
	// +listType=atomic
	Mappings []Mapping `json:"mappings,omitempty"`
}

// Mapping defines a mapping from node selector to ResourceSlice configuration
type Mapping struct {
	// name is a human-readable identifier for this mapping.
	// Used in ResourceSlice naming: test-karpenter-sh-<nodename>-<mapping-name>
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$
	Name string `json:"name,omitempty"`

	// nodeSelectorTerms determines which nodes this mapping applies to.
	// Multiple terms are ORed together (node matches if ANY term matches).
	// Within a term, all requirements must be satisfied (AND logic).
	// Follows standard Kubernetes NodeSelectorTerm format.
	//
	// +kubebuilder:validation:MinItems=1
	// +required
	// +listType=atomic
	NodeSelectorTerms []corev1.NodeSelectorTerm `json:"nodeSelectorTerms,omitempty"`

	// resourceSlice defines the ResourceSlice template to create for matching nodes.
	// The driver will create a ResourceSlice using this template, filling in:
	// - metadata.name (generated from node name and mapping name)
	// - spec.nodeName (from the matching node)
	// - spec.driver (from DRAConfigSpec.Driver)
	//
	// +required
	ResourceSlice ResourceSliceTemplate `json:"resourceSlice,omitzero"`
}

// ResourceSliceTemplate is a template for creating ResourceSlices.
// It's based on resourcev1.ResourceSliceSpec but excludes fields that are auto-populated:
// - nodeName (filled from matching node)
// - driver (filled from DRAConfigSpec.Driver)
type ResourceSliceTemplate struct {
	// nodeSelector is copied directly to ResourceSlice if provided.
	// If omitted, the mapping's NodeSelectorTerms will be used.
	//
	// +optional
	NodeSelector *corev1.NodeSelector `json:"nodeSelector,omitempty"`

	// pool describes the pool that this ResourceSlice belongs to.
	//
	// +required
	Pool resourcev1.ResourcePool `json:"pool,omitempty"`

	// allNodes indicates whether all nodes have access to the devices in this pool.
	// Exactly one of NodeSelector or AllNodes must be set.
	//
	// +optional
	AllNodes *bool `json:"allNodes,omitempty"`

	// devices lists the devices available in this ResourceSlice.
	//
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Devices []resourcev1.Device `json:"devices,omitempty"`
}

// DRAConfigStatus defines the observed state of DRAConfig
type DRAConfigStatus struct {
	// conditions represent the latest available observations of the config's state.
	// Supported condition types:
	// - "Ready": Indicates the config has been successfully processed
	// - "ValidationSucceeded": Indicates the config passed validation
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// resourceSliceCount is the number of ResourceSlices currently created by this config.
	// This may span multiple nodes if the config's selectors match multiple nodes.
	//
	// +optional
	ResourceSliceCount *int32 `json:"resourceSliceCount,omitempty"`

	// nodeCount is the number of nodes currently matched by this config's mappings.
	//
	// +optional
	NodeCount *int32 `json:"nodeCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=draconfig;draconfigs
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.spec.driver`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.nodeCount`
// +kubebuilder:printcolumn:name="ResourceSlices",type=integer,JSONPath=`.status.resourceSliceCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DRAConfig is the Schema for the draconfigs API.
// It configures how the KWOK DRA driver creates ResourceSlices for KWOK nodes.
type DRAConfig struct {
	metav1.TypeMeta `json:",inline"`
	// metadata contains the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of DRAConfig.
	// +required
	Spec DRAConfigSpec `json:"spec,omitzero,omitempty"`
	// status defines the observed state of DRAConfig.
	// +optional
	Status *DRAConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DRAConfigList contains a list of DRAConfig
type DRAConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DRAConfig `json:"items"`
}

// ToResourceSliceSpec converts a ResourceSliceTemplate to a resourcev1.ResourceSliceSpec
// The driver and nodeName fields are filled in by the caller
func (t *ResourceSliceTemplate) ToResourceSliceSpec(driver string) resourcev1.ResourceSliceSpec {
	spec := resourcev1.ResourceSliceSpec{
		Driver:       driver,
		NodeSelector: t.NodeSelector,
		Pool:         t.Pool,
		Devices:      t.Devices,
		AllNodes:     t.AllNodes,
	}

	return spec
}

func init() {
	SchemeBuilder.Register(&DRAConfig{}, &DRAConfigList{})
}
