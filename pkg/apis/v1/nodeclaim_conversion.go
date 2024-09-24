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
	"fmt"
	"strings"

	"github.com/samber/lo"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

// convert v1 to v1beta1
func (in *NodeClaim) ConvertTo(ctx context.Context, to apis.Convertible) error {
	v1beta1NC := to.(*v1beta1.NodeClaim)
	v1beta1NC.ObjectMeta = in.ObjectMeta

	in.Status.convertTo(&v1beta1NC.Status)
	if err := in.Spec.convertTo(&v1beta1NC.Spec, in.Annotations[KubeletCompatibilityAnnotationKey], in.Annotations[NodeClassReferenceAnnotationKey]); err != nil {
		return err
	}
	// Remove the annotations from the v1beta1 NodeClaim on the convert back
	delete(v1beta1NC.Annotations, KubeletCompatibilityAnnotationKey)
	delete(v1beta1NC.Annotations, NodeClassReferenceAnnotationKey)
	// Drop the annotation so when roundtripping from v1, to v1beta1, and back to v1 the migration resource controller can re-annotate it
	delete(v1beta1NC.Annotations, StoredVersionMigratedKey)
	return nil
}

func (in *NodeClaimSpec) convertTo(v1beta1nc *v1beta1.NodeClaimSpec, kubeletAnnotation, nodeClassReferenceAnnotation string) error {
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
	// Convert the NodeClassReference depending on whether the annotation exists
	v1beta1nc.NodeClassRef = &v1beta1.NodeClassReference{}
	if in.NodeClassRef != nil {
		if nodeClassReferenceAnnotation != "" {
			if err := json.Unmarshal([]byte(nodeClassReferenceAnnotation), v1beta1nc.NodeClassRef); err != nil {
				return fmt.Errorf("unmarshaling nodeClassRef annotation, %w", err)
			}
		} else {
			v1beta1nc.NodeClassRef.Name = in.NodeClassRef.Name
			v1beta1nc.NodeClassRef.Kind = in.NodeClassRef.Kind
		}
	}
	if kubeletAnnotation != "" {
		v1beta1kubelet := &v1beta1.KubeletConfiguration{}
		err := json.Unmarshal([]byte(kubeletAnnotation), v1beta1kubelet)
		if err != nil {
			return fmt.Errorf("unmarshaling kubelet config annotation, %w", err)
		}
		v1beta1nc.Kubelet = v1beta1kubelet
	}
	return nil
}

func (in *NodeClaimStatus) convertTo(v1beta1nc *v1beta1.NodeClaimStatus) {
	v1beta1nc.NodeName = in.NodeName
	v1beta1nc.ProviderID = in.ProviderID
	v1beta1nc.ImageID = in.ImageID
	v1beta1nc.Capacity = in.Capacity
	v1beta1nc.Allocatable = in.Allocatable
	v1beta1nc.Conditions = in.Conditions
}

// convert v1beta1 to v1
func (in *NodeClaim) ConvertFrom(ctx context.Context, from apis.Convertible) error {
	v1beta1NC := from.(*v1beta1.NodeClaim)
	in.ObjectMeta = v1beta1NC.ObjectMeta

	in.Status.convertFrom((&v1beta1NC.Status))
	kubeletAnnotation, err := in.Spec.convertFrom(ctx, &v1beta1NC.Spec)
	if err != nil {
		return err
	}
	if kubeletAnnotation == "" {
		in.Annotations = lo.OmitByKeys(in.Annotations, []string{KubeletCompatibilityAnnotationKey})
	} else {
		in.Annotations = lo.Assign(in.Annotations, map[string]string{KubeletCompatibilityAnnotationKey: kubeletAnnotation})
	}
	nodeClassRefAnnotation, err := json.Marshal(v1beta1NC.Spec.NodeClassRef)
	if err != nil {
		return fmt.Errorf("marshaling nodeClassRef annotation, %w", err)
	}
	in.Annotations = lo.Assign(in.Annotations, map[string]string{
		NodeClassReferenceAnnotationKey: string(nodeClassRefAnnotation),
	})
	return in.setExpireAfter(ctx, v1beta1NC)
}

// only need to set expireAfter for v1beta1 to v1
func (in *NodeClaim) setExpireAfter(ctx context.Context, v1beta1nc *v1beta1.NodeClaim) error {
	kubeClient := injection.GetClient(ctx)
	nodePoolName, ok := v1beta1nc.Labels[NodePoolLabelKey]
	if !ok {
		// If we don't have a nodepool for this nodeclaim, there's nothing to look up
		return nil
	}
	nodePool := &NodePool{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		if errors.IsNotFound(err) {
			// If the nodepool doesn't exist, fallback to no expiry, and use the CRD default
			return nil
		}
		return fmt.Errorf("getting nodepool, %w", err)
	}
	in.Spec.ExpireAfter = nodePool.Spec.Template.Spec.ExpireAfter
	return nil
}

func (in *NodeClaimSpec) convertFrom(ctx context.Context, v1beta1nc *v1beta1.NodeClaimSpec) (string, error) {
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

	in.NodeClassRef = &NodeClassReference{}
	if v1beta1nc.NodeClassRef != nil {
		defaultNodeClassGVK := injection.GetNodeClasses(ctx)[0]
		in.NodeClassRef = &NodeClassReference{
			Name:  v1beta1nc.NodeClassRef.Name,
			Kind:  lo.Ternary(v1beta1nc.NodeClassRef.Kind == "", defaultNodeClassGVK.Kind, v1beta1nc.NodeClassRef.Kind),
			Group: lo.Ternary(v1beta1nc.NodeClassRef.APIVersion == "", defaultNodeClassGVK.Group, strings.Split(v1beta1nc.NodeClassRef.APIVersion, "/")[0]),
		}
	}

	if v1beta1nc.Kubelet != nil {
		kubelet, err := json.Marshal(v1beta1nc.Kubelet)
		if err != nil {
			return "", fmt.Errorf("marshaling kubelet config annotation, %w", err)
		}
		return string(kubelet), nil
	}
	return "", nil
}

func (in *NodeClaimStatus) convertFrom(v1beta1nc *v1beta1.NodeClaimStatus) {
	in.NodeName = v1beta1nc.NodeName
	in.ProviderID = v1beta1nc.ProviderID
	in.ImageID = v1beta1nc.ImageID
	in.Capacity = v1beta1nc.Capacity
	in.Allocatable = v1beta1nc.Allocatable
	in.Conditions = v1beta1nc.Conditions
}
