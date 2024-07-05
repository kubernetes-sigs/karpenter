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

package v1

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"knative.dev/pkg/apis"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

// Convert v1 NodePool to v1beta1 NodePool
func (in *NodePool) ConvertTo(ctx context.Context, to apis.Convertible) error {
	v1beta1NP := to.(*v1beta1.NodePool)
	v1beta1NP.ObjectMeta = in.ObjectMeta

	// Convert v1 status
	v1beta1NP.Status.Resources = in.Status.Resources
	v1beta1NP.Status.Conditions = in.Status.Conditions
	return in.Spec.convertTo(ctx, &v1beta1NP.Spec, in.Annotations[KubeletCompatabilityAnnotationKey])
}

func (in *NodePoolSpec) convertTo(ctx context.Context, v1beta1np *v1beta1.NodePoolSpec, kubeletAnnotation string) error {
	v1beta1np.Weight = in.Weight
	v1beta1np.Limits = v1beta1.Limits(in.Limits)
	in.Disruption.convertTo(&v1beta1np.Disruption)
	return in.Template.convertTo(ctx, &v1beta1np.Template, kubeletAnnotation)
}

func (in *Disruption) convertTo(v1beta1np *v1beta1.Disruption) {
	v1beta1np.ConsolidateAfter = (*v1beta1.NillableDuration)(in.ConsolidateAfter)
	v1beta1np.ConsolidationPolicy = v1beta1.ConsolidationPolicy(in.ConsolidationPolicy)
	v1beta1np.ExpireAfter = v1beta1.NillableDuration(in.ExpireAfter)
	v1beta1np.Budgets = lo.Map(in.Budgets, func(v1budget Budget, _ int) v1beta1.Budget {
		return v1beta1.Budget{
			Reasons: lo.Map(v1budget.Reasons, func(reason DisruptionReason, _ int) v1beta1.DisruptionReason {
				return v1beta1.DisruptionReason(reason)
			}),
			Nodes:    v1budget.Nodes,
			Schedule: v1budget.Schedule,
			Duration: v1budget.Duration,
		}
	})
}

func (in *NodeClaimTemplate) convertTo(ctx context.Context, v1beta1np *v1beta1.NodeClaimTemplate, kubeletAnnotation string) error {
	v1beta1np.ObjectMeta = v1beta1.ObjectMeta(in.ObjectMeta)
	v1beta1np.Spec.Taints = in.Spec.Taints
	v1beta1np.Spec.StartupTaints = in.Spec.StartupTaints
	v1beta1np.Spec.Requirements = lo.Map(in.Spec.Requirements, func(v1Requirements NodeSelectorRequirementWithMinValues, _ int) v1beta1.NodeSelectorRequirementWithMinValues {
		return v1beta1.NodeSelectorRequirementWithMinValues{
			NodeSelectorRequirement: v1.NodeSelectorRequirement{
				Key:      v1Requirements.Key,
				Values:   v1Requirements.Values,
				Operator: v1Requirements.Operator,
			},
			MinValues: v1Requirements.MinValues,
		}
	})

	nodeClasses := injection.GetNodeClasses(ctx)
	// We are sorting the supported nodeclass, so that we are able to consistently find the same GVK,
	// if multiple version of a nodeclass are supported
	sort.Slice(nodeClasses, func(i int, j int) bool {
		if nodeClasses[i].Group != nodeClasses[j].Group {
			return nodeClasses[i].Group < nodeClasses[j].Group
		}
		if nodeClasses[i].Version != nodeClasses[j].Version {
			return nodeClasses[i].Version < nodeClasses[j].Version
		}
		return nodeClasses[i].Kind < nodeClasses[j].Kind
	})
	matchingNodeClass, found := lo.Find(nodeClasses, func(nc schema.GroupVersionKind) bool {
		return nc.Kind == in.Spec.NodeClassRef.Kind && nc.Group == in.Spec.NodeClassRef.Group
	})
	v1beta1np.Spec.NodeClassRef = &v1beta1.NodeClassReference{
		Kind:       in.Spec.NodeClassRef.Kind,
		Name:       in.Spec.NodeClassRef.Name,
		APIVersion: lo.Ternary(found, matchingNodeClass.GroupVersion().String(), ""),
	}

	if kubeletAnnotation != "" {
		v1beta1kubelet := &v1beta1.KubeletConfiguration{}
		err := json.Unmarshal([]byte(kubeletAnnotation), v1beta1kubelet)
		if err != nil {
			return err
		}
		v1beta1np.Spec.Kubelet = v1beta1kubelet
	}
	return nil
}

// Convert v1beta1 NodePool to V1 NodePool
func (in *NodePool) ConvertFrom(ctx context.Context, v1beta1np apis.Convertible) error {
	v1beta1NP := v1beta1np.(*v1beta1.NodePool)
	in.ObjectMeta = v1beta1NP.ObjectMeta

	// Convert v1beta1 status
	in.Status.Resources = v1beta1NP.Status.Resources
	in.Status.Conditions = v1beta1NP.Status.Conditions

	kubeletAnnotation, err := in.Spec.convertFrom(ctx, &v1beta1NP.Spec)
	if err != nil {
		return err
	}
	in.Annotations = lo.Assign(in.Annotations, map[string]string{KubeletCompatabilityAnnotationKey: kubeletAnnotation})
	return nil
}

func (in *NodePoolSpec) convertFrom(ctx context.Context, v1beta1np *v1beta1.NodePoolSpec) (string, error) {
	in.Weight = v1beta1np.Weight
	in.Limits = Limits(v1beta1np.Limits)
	in.Disruption.convertFrom(&v1beta1np.Disruption)
	return in.Template.convertFrom(ctx, &v1beta1np.Template)
}

func (in *Disruption) convertFrom(v1beta1np *v1beta1.Disruption) {
	in.ConsolidateAfter = (*NillableDuration)(v1beta1np.ConsolidateAfter)
	in.ConsolidationPolicy = ConsolidationPolicy(v1beta1np.ConsolidationPolicy)
	in.ExpireAfter = NillableDuration(v1beta1np.ExpireAfter)
	in.Budgets = lo.Map(v1beta1np.Budgets, func(v1beta1budget v1beta1.Budget, _ int) Budget {
		return Budget{
			Reasons: lo.Map(v1beta1budget.Reasons, func(reason v1beta1.DisruptionReason, _ int) DisruptionReason {
				return DisruptionReason(reason)
			}),
			Nodes:    v1beta1budget.Nodes,
			Schedule: v1beta1budget.Schedule,
			Duration: v1beta1budget.Duration,
		}
	})
}

func (in *NodeClaimTemplate) convertFrom(ctx context.Context, v1beta1np *v1beta1.NodeClaimTemplate) (string, error) {
	in.ObjectMeta = ObjectMeta(v1beta1np.ObjectMeta)
	in.Spec.Taints = v1beta1np.Spec.Taints
	in.Spec.StartupTaints = v1beta1np.Spec.StartupTaints
	in.Spec.Requirements = lo.Map(v1beta1np.Spec.Requirements, func(v1beta1Requirements v1beta1.NodeSelectorRequirementWithMinValues, _ int) NodeSelectorRequirementWithMinValues {
		return NodeSelectorRequirementWithMinValues{
			NodeSelectorRequirement: v1.NodeSelectorRequirement{
				Key:      v1beta1Requirements.Key,
				Values:   v1beta1Requirements.Values,
				Operator: v1beta1Requirements.Operator,
			},
			MinValues: v1beta1Requirements.MinValues,
		}
	})

	nodeclasses := injection.GetNodeClasses(ctx)
	in.Spec.NodeClassRef = &NodeClassReference{
		Name:  v1beta1np.Spec.NodeClassRef.Name,
		Kind:  lo.Ternary(v1beta1np.Spec.NodeClassRef.Kind == "", nodeclasses[0].Kind, v1beta1np.Spec.NodeClassRef.Kind),
		Group: lo.Ternary(v1beta1np.Spec.NodeClassRef.APIVersion == "", nodeclasses[0].Group, strings.Split(v1beta1np.Spec.NodeClassRef.APIVersion, "/")[0]),
	}

	defaultNodeClassGVK := injection.GetNodeClasses(ctx)[0]
	nodeclassGroupVersion, err := schema.ParseGroupVersion(v1beta1np.Spec.NodeClassRef.APIVersion)
	if err != nil {
		return "", err
	}
	in.Spec.NodeClassRef = &NodeClassReference{
		Name:  v1beta1np.Spec.NodeClassRef.Name,
		Kind:  lo.Ternary(v1beta1np.Spec.NodeClassRef.Kind == "", defaultNodeClassGVK.Kind, v1beta1np.Spec.NodeClassRef.Kind),
		Group: lo.Ternary(v1beta1np.Spec.NodeClassRef.APIVersion == "", defaultNodeClassGVK.Group, nodeclassGroupVersion.Group),
	}

	kubelet, err := json.Marshal(v1beta1np.Spec.Kubelet)
	if err != nil {
		return "", err
	}
	return string(kubelet), nil
}
