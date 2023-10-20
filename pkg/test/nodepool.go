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
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
)

// NodePool creates a test NodePool with defaults that can be overridden by overrides.
// Overrides are applied in order, with a last write wins semantic.
func NodePool(overrides ...v1beta1.NodePool) *v1beta1.NodePool {
	override := v1beta1.NodePool{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	if override.Spec.Limits == nil {
		override.Spec.Limits = v1beta1.Limits(v1.ResourceList{v1.ResourceCPU: resource.MustParse("2000")})
	}
	if override.Spec.Template.Spec.NodeClassRef == nil {
		override.Spec.Template.Spec.NodeClassRef = &v1beta1.NodeClassReference{
			Name: "default",
		}
	}
	if override.Spec.Template.Spec.Requirements == nil {
		override.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{}
	}
	np := &v1beta1.NodePool{
		ObjectMeta: ObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
		Status:     override.Status,
	}
	np.Spec.Template.ObjectMeta = TemplateObjectMeta(np.Spec.Template.ObjectMeta)
	return np
}
