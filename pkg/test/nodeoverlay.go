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

package test

import (
	"fmt"

	"github.com/imdario/mergo"
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
)

// NodeOverlay creates a test NodeOverlay with defaults that can be overridden by overrides.
// Overrides are applied in order, with a last write wins semantic.
func NodeOverlay(overrides ...v1alpha1.NodeOverlay) *v1alpha1.NodeOverlay {
	override := v1alpha1.NodeOverlay{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	if override.Spec.Requirements == nil {
		override.Spec.Requirements = []corev1.NodeSelectorRequirement{}
	}
	no := &v1alpha1.NodeOverlay{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
		Status:     override.Status,
	}
	return no
}
