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

	// pools defines how KWOK nodes map to ResourceSlices.
	// Each pool specifies node selectors and the devices to expose on matching nodes.
	// Multiple pools allow different device configurations for different node types.
	// Pool names are auto-generated as <driver>/<node> to prevent overlap across nodes.
	//
	// +kubebuilder:validation:MinItems=1
	// +required
	// +listType=atomic
	Pools []Pool `json:"pools,omitempty"`
}

// Pool defines a pool of devices for nodes matching the selector.
// Each resourceSlice entry within a pool becomes one ResourceSlice per matching node.
// All ResourceSlices in a pool share the same auto-generated pool name <driver>/<node>.
type Pool struct {
	// name is a human-readable identifier for this pool.
	// Used in ResourceSlice naming: <driver-sanitized>-<nodename>-<pool-name>[-<index>]
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$
	Name string `json:"name,omitempty"`

	// nodeSelectorTerms determines which nodes this pool applies to.
	// Multiple terms are ORed together (node matches if ANY term matches).
	// Within a term, all requirements must be satisfied (AND logic).
	// Follows standard Kubernetes NodeSelectorTerm format.
	//
	// +kubebuilder:validation:MinItems=1
	// +required
	// +listType=atomic
	NodeSelectorTerms []corev1.NodeSelectorTerm `json:"nodeSelectorTerms,omitempty"`

	// resourceSlices defines one or more ResourceSlice templates for this pool.
	// Each entry becomes a separate ResourceSlice on each matching node.
	// This allows splitting devices across multiple ResourceSlices within the same pool,
	// which is useful for simulating real drivers that partition devices across slices.
	// All entries share the same auto-generated pool name with ResourceSliceCount
	// set to the total number of entries.
	//
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	ResourceSlices []ResourceSliceTemplate `json:"resourceSlices,omitempty"`
}

// ResourceSliceTemplate defines the devices that will be placed into a single ResourceSlice.
type ResourceSliceTemplate struct {
	// devices lists the devices for this ResourceSlice.
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

	// nodeCount is the number of nodes currently matched by this config's pools.
	//
	// +optional
	NodeCount *int32 `json:"nodeCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=draconfig;draconfigs
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

func init() {
	SchemeBuilder.Register(&DRAConfig{}, &DRAConfigList{})
}
