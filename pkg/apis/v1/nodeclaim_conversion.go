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
	"strings"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

func (in *NodeClaim) ConvertTo(ctx context.Context, to apis.Convertible) error {
	v1beta1NC := to.(*v1beta1.NodeClaim)
	v1beta1NC.Name = in.Name
	v1beta1NC.UID = in.UID
	v1beta1NC.Labels = in.Labels
	v1beta1NC.Annotations = in.Annotations

	// Updating the drift hash on the nodeclaim
	v1OwnerNodePool, err := in.getV1NodePoolDriftHash(ctx)
	if err != nil {
		return err
	}
	temp := map[string]string{
		NodePoolHashAnnotationKey:        v1OwnerNodePool.Annotations[NodePoolHashAnnotationKey],
		NodePoolHashVersionAnnotationKey: v1OwnerNodePool.Annotations[NodePoolHashVersionAnnotationKey],
	}
	v1beta1NC.Annotations = lo.Assign(v1beta1NC.Annotations, temp)

	in.Spec.convertTo(ctx, &v1beta1NC.Spec)
	in.Status.convertTo((&v1beta1NC.Status))
	return nil
}

func (in *NodeClaimSpec) convertTo(ctx context.Context, v1beta1nc *v1beta1.NodeClaimSpec) {
	v1beta1nc.Taints = in.Taints
	v1beta1nc.StartupTaints = in.StartupTaints
	v1beta1nc.Resources = v1beta1.ResourceRequirements(in.Resources)
	v1beta1nc.Requirements = lo.Map(in.Requirements, func(v1Requirements NodeSelectorRequirementWithMinValues, _ int) v1beta1.NodeSelectorRequirementWithMinValues {
		return v1beta1.NodeSelectorRequirementWithMinValues{
			NodeSelectorRequirement: v1.NodeSelectorRequirement{
				Key:      v1Requirements.Key,
				Operator: v1Requirements.Operator,
				Values:   v1Requirements.Values,
			},
			MinValues: v1Requirements.MinValues,
		}
	})

	if in.NodeClassRef != nil {
		nodeclass, found := lo.Find(injection.GetNodeClasses(ctx), func(nc schema.GroupVersionKind) bool {
			return nc.Kind == in.NodeClassRef.Kind && nc.Group == in.NodeClassRef.Group
		})
		v1beta1nc.NodeClassRef = &v1beta1.NodeClassReference{
			Kind:       in.NodeClassRef.Kind,
			Name:       in.NodeClassRef.Name,
			APIVersion: lo.Ternary(found, nodeclass.GroupVersion().String(), ""),
		}
	}

	// Need to implement Kubelet Conversion
}

func (in *NodeClaimStatus) convertTo(v1beta1nc *v1beta1.NodeClaimStatus) {
	v1beta1nc.NodeName = in.NodeName
	v1beta1nc.ProviderID = in.ProviderID
	v1beta1nc.ImageID = in.ImageID
	v1beta1nc.Capacity = in.Capacity
	v1beta1nc.Allocatable = in.Allocatable
	v1beta1nc.Conditions = in.Conditions
}

func (in *NodeClaim) ConvertFrom(ctx context.Context, from apis.Convertible) error {
	v1beta1NC := from.(*v1beta1.NodeClaim)
	in.Name = v1beta1NC.Name
	in.UID = v1beta1NC.UID
	in.Labels = v1beta1NC.Labels
	in.Annotations = v1beta1NC.Annotations

	in.Spec.convertFrom(ctx, &v1beta1NC.Spec)
	// Updating the drift hash on the nodeclaim
	v1beta1OwnerNodePool, err := getV1Beta1NodePoolDriftHash(ctx, v1beta1NC)
	if err != nil {
		return err
	}
	in.Annotations = lo.Assign(in.Annotations, map[string]string{
		NodePoolHashAnnotationKey:        v1beta1OwnerNodePool.Annotations[v1beta1.NodePoolHashAnnotationKey],
		NodePoolHashVersionAnnotationKey: v1beta1OwnerNodePool.Annotations[v1beta1.NodePoolHashVersionAnnotationKey],
	})

	in.Status.convertFrom((&v1beta1NC.Status))
	return nil
}

func (in *NodeClaimSpec) convertFrom(ctx context.Context, v1beta1nc *v1beta1.NodeClaimSpec) {
	in.Taints = v1beta1nc.Taints
	in.StartupTaints = v1beta1nc.StartupTaints
	in.Resources = ResourceRequirements(v1beta1nc.Resources)
	in.Requirements = lo.Map(v1beta1nc.Requirements, func(v1beta1Requirements v1beta1.NodeSelectorRequirementWithMinValues, _ int) NodeSelectorRequirementWithMinValues {
		return NodeSelectorRequirementWithMinValues{
			NodeSelectorRequirement: v1.NodeSelectorRequirement{
				Key:      v1beta1Requirements.Key,
				Operator: v1beta1Requirements.Operator,
				Values:   v1beta1Requirements.Values,
			},
			MinValues: v1beta1Requirements.MinValues,
		}
	})

	nodeclasses := injection.GetNodeClasses(ctx)
	if v1beta1nc.NodeClassRef != nil {
		in.NodeClassRef = &NodeClassReference{
			Name:  v1beta1nc.NodeClassRef.Name,
			Kind:  lo.Ternary(v1beta1nc.NodeClassRef.Kind == "", nodeclasses[0].Kind, v1beta1nc.NodeClassRef.Kind),
			Group: lo.Ternary(v1beta1nc.NodeClassRef.APIVersion == "", nodeclasses[0].Group, strings.Split(v1beta1nc.NodeClassRef.APIVersion, "/")[0]),
		}
	}

	// Need to implement Kubelet Conversion
}

func (in *NodeClaimStatus) convertFrom(v1beta1nc *v1beta1.NodeClaimStatus) {
	in.NodeName = v1beta1nc.NodeName
	in.ProviderID = v1beta1nc.ProviderID
	in.ImageID = v1beta1nc.ImageID
	in.Capacity = v1beta1nc.Capacity
	in.Allocatable = v1beta1nc.Allocatable
	in.Conditions = v1beta1nc.Conditions
}

func (in *NodeClaim) getV1NodePoolDriftHash(ctx context.Context) (*NodePool, error) {
	client := injection.GetClient(ctx)

	nodepool := &NodePool{}
	err := client.Get(ctx, types.NamespacedName{Name: in.Labels[NodePoolLabelKey]}, nodepool)
	if err != nil {
		return nil, err
	}
	return nodepool, nil
}

func getV1Beta1NodePoolDriftHash(ctx context.Context, v1beta1nc *v1beta1.NodeClaim) (*v1beta1.NodePool, error) {
	client := injection.GetClient(ctx)

	nodepool := &v1beta1.NodePool{}
	err := client.Get(ctx, types.NamespacedName{Name: v1beta1nc.Labels[NodePoolLabelKey]}, nodepool)
	if err != nil {
		return nil, err
	}
	return nodepool, nil
}
