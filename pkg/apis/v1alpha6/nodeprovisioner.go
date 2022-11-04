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
	"fmt"
	"sort"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/ptr"
)

// NodeProvisionerSpec is the top level provisioner specification. NodeProvisioners
// launch nodes in response to pods that are unschedulable. A single provisioner
// is capable of managing a diverse set of nodes. Node properties are determined
// from a combination of provisioner and pod scheduling constraints.
type NodeProvisionerSpec struct {
	// Template of the machines launched by this provisioner
	Template Machine `json:"template,omitempty"`
	// UnderutilizationTTL is the number of seconds the controller will wait
	// before attempting to delete a node, measured from when the node is
	// detected to be underutilized.
	//
	// Termination due to underutilization is disabled if this field is not set.
	// +optional
	UnderutilizationTTL *int64 `json:"underutilizationTTL,omitempty"`
	// TTLSecondsUntilExpired is the number of seconds the controller will wait
	// before terminating a node, measured from when the node is created. This
	// is useful to implement features like eventually consistent node upgrade,
	// memory leak protection, and disruption testing.
	//
	// Termination due to expiration is disabled if this field is not set.
	// +optional
	ExpirationTTL *int64 `json:"expirationTTL,omitempty"`
	// Limits define a set of bounds for provisioning capacity.
	Limits *Limits `json:"limits,omitempty"`
	// Weight is the priority given to the provisioner during scheduling. A higher
	// numerical weight indicates that this provisioner will be ordered
	// ahead of other provisioners with lower weights. A provisioner with no weight
	// will be treated as if it is a provisioner with a weight of 0.
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=100
	// +optional
	Weight *int32 `json:"weight,omitempty"`
}

// Kubelet defines args to be used when configuring kubelet on provisioned nodes.
// They are a subset of the upstream types, recognizing not all options may be supported.
// Wherever possible, the types and names should reflect the upstream kubelet types.
// https://pkg.go.dev/k8s.io/kubelet/config/v1beta1#KubeletConfiguration
// https://github.com/kubernetes/kubernetes/blob/9f82d81e55cafdedab619ea25cabf5d42736dacf/cmd/kubelet/app/options/options.go#L53
type Kubelet struct {
	// clusterDNS is a list of IP addresses for the cluster DNS server.
	// Note that not all providers may use all addresses.
	//+optional
	ClusterDNS []string `json:"clusterDNS,omitempty"`
	// ContainerRuntime is the container runtime to be used with your worker nodes.
	// +optional
	ContainerRuntime *string `json:"containerRuntime,omitempty"`
	// MaxPods is an override for the maximum number of pods that can run on
	// a worker node instance.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	MaxPods *int32 `json:"maxPods,omitempty"`
	// PodsPerCore is an override for the number of pods that can run on a worker node
	// instance based on the number of cpu cores. This value cannot exceed MaxPods, so, if
	// MaxPods is a lower value, that value will be used.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	PodsPerCore *int32 `json:"podsPerCore,omitempty"`
	// SystemReserved contains resources reserved for OS system daemons and kernel memory.
	// +optional
	SystemReserved v1.ResourceList `json:"systemReserved,omitempty"`
	// KubeReserved contains resources reserved for Kubernetes system components.
	// +optional
	KubeReserved v1.ResourceList `json:"kubeReserved,omitempty"`
	// EvictionHard is the map of signal names to quantities that define hard eviction thresholds
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty"`
	// EvictionSoft is the map of signal names to quantities that define soft eviction thresholds
	// +optional
	EvictionSoft map[string]string `json:"evictionSoft,omitempty"`
	// EvictionSoftGracePeriod is the map of signal names to quantities that define grace periods for each eviction signal
	// +optional
	EvictionSoftGracePeriod map[string]metav1.Duration `json:"evictionSoftGracePeriod,omitempty"`
	// EvictionMaxPodGracePeriod is the maximum allowed grace period (in seconds) to use when terminating pods in
	// response to soft eviction thresholds being met.
	// +optional
	EvictionMaxPodGracePeriod *int32 `json:"evictionMaxPodGracePeriod,omitempty"`
}

// Limits define bounds on the resources being provisioned by Karpenter
type Limits struct {
	// Resources contains all the allocatable resources that Karpenter supports for limiting.
	Resources v1.ResourceList `json:"resources,omitempty"`
}

func (l *Limits) ExceededBy(resources v1.ResourceList) error {
	if l == nil || l.Resources == nil {
		return nil
	}
	for resourceName, usage := range resources {
		if limit, ok := l.Resources[resourceName]; ok {
			if usage.Cmp(limit) >= 0 {
				return fmt.Errorf("%s resource usage of %v exceeds limit of %v", resourceName, usage.AsDec(), limit.AsDec())
			}
		}
	}
	return nil
}

// NodeProvisioner is the Schema for the NodeProvisioner API
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=nodeprovisioners,scope=Cluster,categories=karpenter
// +kubebuilder:subresource:status
type NodeProvisioner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeProvisionerSpec   `json:"spec,omitempty"`
	Status NodeProvisionerStatus `json:"status,omitempty"`
}

// NodeProvisionerList contains a list of Provisioner
// +kubebuilder:object:root=true
type NodeProvisionerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeProvisioner `json:"items"`
}

// OrderByWeight orders the provisioners in the NodeProvisionerList
// by their priority weight in-place
func (pl *NodeProvisionerList) OrderByWeight() {
	sort.Slice(pl.Items, func(a, b int) bool {
		return ptr.Int32Value(pl.Items[a].Spec.Weight) > ptr.Int32Value(pl.Items[b].Spec.Weight)
	})
}
