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
	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
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
	override.ObjectMeta.Labels = lo.Assign(override.ObjectMeta.Labels, map[string]string{
		v1alpha5.MachineNameLabelKey: override.Name,
	})
	return &v1alpha5.Machine{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
		Status:     override.Status,
	}
}

func MarkMachineReady(machine *v1alpha5.Machine) {
	machine.StatusConditions().MarkTrue(v1alpha5.MachineCreated)
	machine.StatusConditions().MarkTrue(v1alpha5.MachineRegistered)
	machine.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
}
