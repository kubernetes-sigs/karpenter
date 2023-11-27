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

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha5"
)

// Machine creates a test machine with defaults that can be overridden by MachineOptions.
// Overrides are applied in order, with a last write wins semantic.
func Machine(overrides ...v1alpha5.Machine) *v1alpha5.Machine {
	override := v1alpha5.Machine{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	if override.Status.ProviderID == "" {
		override.Status.ProviderID = RandomProviderID()
	}
	return &v1alpha5.Machine{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
		Status:     override.Status,
	}
}

func MachineAndNode(overrides ...v1alpha5.Machine) (*v1alpha5.Machine, *v1.Node) {
	m := Machine(overrides...)
	return m, MachineLinkedNode(m)
}

// MachinesAndNodes creates homogeneous groups of machines and nodes based on the passed in options, evenly divided by the total machines requested
func MachinesAndNodes(total int, options ...v1alpha5.Machine) ([]*v1alpha5.Machine, []*v1.Node) {
	machines := make([]*v1alpha5.Machine, total)
	nodes := make([]*v1.Node, total)
	for _, opts := range options {
		for i := 0; i < total/len(options); i++ {
			machine, node := MachineAndNode(opts)
			machines[i] = machine
			nodes[i] = node
		}
	}
	return machines, nodes
}
