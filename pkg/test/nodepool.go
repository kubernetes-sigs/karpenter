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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
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

// ReplaceRequirements any current requirements on the passed through NodePool with the passed in requirements
// If any of the keys match between the existing requirements and the new requirements, the new requirement with the same
// key will replace the old requirement with that key
func ReplaceRequirements(nodePool *v1beta1.NodePool, reqs ...v1.NodeSelectorRequirement) *v1beta1.NodePool {
	keys := sets.New[string](lo.Map(reqs, func(r v1.NodeSelectorRequirement, _ int) string { return r.Key })...)
	nodePool.Spec.Template.Spec.Requirements = lo.Reject(nodePool.Spec.Template.Spec.Requirements, func(r v1.NodeSelectorRequirement, _ int) bool {
		return keys.Has(r.Key)
	})
	nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements, reqs...)
	return nodePool
}
