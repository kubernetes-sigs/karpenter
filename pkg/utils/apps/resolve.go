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

package apps

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

// PodTemplateResult holds the resolved data from a PodTemplate lookup.
type PodTemplateResult struct {
	PodSpec    corev1.PodSpec
	Name       string
	Generation int64
}

// ResolvePodTemplateRef fetches a PodTemplate by name and namespace.
func ResolvePodTemplateRef(ctx context.Context, c client.Client, name, namespace string) (*PodTemplateResult, error) {
	pt := &corev1.PodTemplate{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pt); err != nil {
		return nil, err
	}
	return &PodTemplateResult{
		PodSpec:    pt.Template.Spec,
		Name:       pt.Name,
		Generation: pt.Generation,
	}, nil
}

// ScalableRefResult holds the resolved data from a scalableRef lookup.
type ScalableRefResult struct {
	PodSpec          corev1.PodSpec
	ScalableReplicas int32
}

// ResolveScalableRef fetches the workload referenced by a ScalableRef and returns
// its pod spec and replica count.
func ResolveScalableRef(ctx context.Context, c client.Client, ref *autoscalingv1beta1.ScalableRef, namespace string) (*ScalableRefResult, error) {
	group := ref.APIGroup
	if group == "" {
		group = "apps"
	}
	if group != "apps" {
		return nil, fmt.Errorf("unsupported scalableRef API group %q", ref.APIGroup)
	}
	key := types.NamespacedName{Name: ref.Name, Namespace: namespace}

	switch ref.Kind {
	case autoscalingv1beta1.KindDeployment:
		obj := &appsv1.Deployment{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return &ScalableRefResult{PodSpec: obj.Spec.Template.Spec, ScalableReplicas: lo.FromPtrOr(obj.Spec.Replicas, 1)}, nil
	case autoscalingv1beta1.KindStatefulSet:
		obj := &appsv1.StatefulSet{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return &ScalableRefResult{PodSpec: obj.Spec.Template.Spec, ScalableReplicas: lo.FromPtrOr(obj.Spec.Replicas, 1)}, nil
	case autoscalingv1beta1.KindReplicaSet:
		obj := &appsv1.ReplicaSet{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return &ScalableRefResult{PodSpec: obj.Spec.Template.Spec, ScalableReplicas: lo.FromPtrOr(obj.Spec.Replicas, 1)}, nil
	default:
		return nil, fmt.Errorf("unsupported kind %q", ref.Kind)
	}
}
