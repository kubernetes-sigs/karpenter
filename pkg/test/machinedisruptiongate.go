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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
)

// MachineDisruptionGate creates a test machine disruption gate with defaults that can be overridden by MachineDisruptionGate overrides.
// Overrides are applied in order, with a last write wins semantic.
func MachineDisruptionGate(overrides ...v1alpha5.MachineDisruptionGate) *v1alpha5.MachineDisruptionGate {
	override := v1alpha5.MachineDisruptionGate{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	if override.Spec.Selector == nil {
		override.Spec.Selector = &metav1.LabelSelector{}
	}

	return &v1alpha5.MachineDisruptionGate{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
		Status:     override.Status,
	}
}
