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

package test

import (
	"fmt"

	"github.com/imdario/mergo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha1"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
)

// MachineOptions customizes a Machine.
type MachineOptions struct {
	metav1.ObjectMeta
	Requirements  []v1.NodeSelectorRequirement
	Kubelet       *v1alpha5.KubeletConfiguration
	Taints        []v1.Taint
	StartupTaints []v1.Taint
	Capacity      v1.ResourceList
	Allocatable   v1.ResourceList
}

// Machine creates a test machine with defaults that can be overridden by MachineOptions.
// Overrides are applied in order, with a last write wins semantic.
func Machine(overrides ...MachineOptions) *v1alpha1.Machine {
	options := MachineOptions{}
	for _, opts := range overrides {
		if err := mergo.Merge(&options, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("Failed to merge provisioner options: %s", err))
		}
	}
	if options.Name == "" {
		options.Name = RandomName()
	}
	return &v1alpha1.Machine{
		ObjectMeta: ObjectMeta(options.ObjectMeta),
		Spec: v1alpha1.MachineSpec{
			Requirements:  options.Requirements,
			Kubelet:       options.Kubelet,
			Taints:        options.Taints,
			StartupTaints: options.StartupTaints,
		},
		Status: v1alpha1.MachineStatus{
			Capacity:    options.Capacity,
			Allocatable: options.Allocatable,
		},
	}
}
