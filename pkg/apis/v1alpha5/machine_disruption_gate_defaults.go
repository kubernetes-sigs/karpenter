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

package v1alpha5

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetDefaults for the MachineDisruptionGate
func (m *MachineDisruptionGate) SetDefaults(ctx context.Context) {
	m.Spec.Selector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      ProvisionerNameLabelKey,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}
}
