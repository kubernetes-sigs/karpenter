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

package nodeoverlay

import (
	"context"
	"fmt"
	"sort"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	lop "github.com/samber/lo/parallel"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func GetInstanceTypes(ctx context.Context, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, np *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	its, err := pullInstanceTypes(ctx, cloudProvider, np)
	if err != nil {
		return []*cloudprovider.InstanceType{}, err
	}
	// The additional requirements will be added to the instance type during scheduling simulation
	// Since getting instance types is done on a NodePool level, these requirnements were always assumed
	// to be allowed with these instance types.
	addNodePoolRequirements(np, its)
	its, err = OverlayInstanceTypes(ctx, kubeClient, its)
	if err != nil {
		return []*cloudprovider.InstanceType{}, err
	}
	removeNodePoolRequirements(np, its)
	return its, nil
}

func OverlayInstanceTypes(ctx context.Context, kubeClient client.Client, instanceTypes []*cloudprovider.InstanceType) ([]*cloudprovider.InstanceType, error) {
	overlays := &v1alpha1.NodeOverlayList{}
	err := kubeClient.List(ctx, overlays)
	if err != nil {
		return []*cloudprovider.InstanceType{}, err
	}

	// Only apply overlays that have passed runtime validation and know there have not been
	// any updates to the spec of the overlay field
	validOverlays := lo.Filter(overlays.Items, func(o v1alpha1.NodeOverlay, _ int) bool {
		condition := o.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded)
		return condition.IsTrue() && condition.ObservedGeneration == o.Generation
	})

	// We order the overlays from largest to smallest, as we have already done validation
	// we can be sure that we will not be blocked due to invalid or conflicting overlays
	sort.Slice(validOverlays, func(x int, y int) bool {
		return lo.FromPtr(validOverlays[x].Spec.Weight) > lo.FromPtr(validOverlays[y].Spec.Weight)
	})
	work := []func(overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType){
		overlayCapacityOnInstanceTypes,
		overlayPriceOnInstanceTypes,
	}
	lop.ForEach(work, func(f func(overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType), i int) {
		f(validOverlays, instanceTypes)
	})

	return instanceTypes, nil
}

func overlayCapacityOnInstanceTypes(overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType) {
	for _, overlay := range overlays {
		overlaySelector := scheduling.NewRequirements()
		overlaySelector.Add(scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...).Values()...)

		for _, it := range its {
			if it.Requirements.IsCompatible(overlaySelector) {
				it.Capacity = lo.Assign(it.Capacity, overlay.Spec.Capacity)
				it.ApplyResourceOverlay()
			}
		}
	}
}

func overlayPriceOnInstanceTypes(overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType) {
	// we will only update instance types once
	overriddenInstanceType := map[string][]string{}
	for _, overlay := range overlays {
		// if price or price adjustment is not defined, then we should skip the overlay
		if overlay.Spec.Price == nil && overlay.Spec.PriceAdjustment == nil {
			continue
		}
		overlaySelector := scheduling.NewRequirements()
		overlaySelector.Add(scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...).Values()...)

		for _, it := range its {
			if !it.Requirements.IsCompatible(overlaySelector) {
				continue
			}
			if _, ok := overriddenInstanceType[it.Name]; !ok {
				overriddenInstanceType[it.Name] = []string{}
			}

			for _, of := range it.Offerings {
				offeringRequirementsHash := fmt.Sprint(lo.Must(hashstructure.Hash(of.Requirements.String(), hashstructure.FormatV2, &hashstructure.HashOptions{})))
				if overlaySelector.IsCompatible(of.Requirements, scheduling.AllowUndefinedWellKnownLabels) && !lo.Contains(overriddenInstanceType[it.Name], offeringRequirementsHash) {
					of.ApplyOverlay()
					overriddenInstanceType[it.Name] = append(overriddenInstanceType[it.Name], offeringRequirementsHash)
					of.Price = overlay.AdjustedPrice(of.Price)
				}
			}
		}
	}
}

// As the instance types values are returned are pointers, we need to make sure that we are not
// updating the instances returned by the cloud provider. We need to get a fresh copy of the instances types
// Such that we only overlay currently defined set of overrides
func pullInstanceTypes(ctx context.Context, cp cloudprovider.CloudProvider, nodePool *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	its, err := cp.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		return []*cloudprovider.InstanceType{}, err
	}

	// Since we are altering the instance type data we need to copy the instance types into a new struct
	// The main things that have been copied are the offerings, capacity and overhead
	return lo.Map(its, func(it *cloudprovider.InstanceType, _ int) *cloudprovider.InstanceType {
		// The additional requirements will be added to the instance type during scheduling simulation
		// Since getting instance types is done on a NodePool level, these requirnements were always assumed
		// to be allowed with these instance types.
		return &cloudprovider.InstanceType{
			Name:         it.Name,
			Requirements: scheduling.NewRequirements(it.Requirements.Values()...),
			Offerings:    copyOfferings(it.Offerings),
			Capacity:     it.Capacity.DeepCopy(),
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved:      it.Overhead.KubeReserved.DeepCopy(),
				SystemReserved:    it.Overhead.SystemReserved.DeepCopy(),
				EvictionThreshold: it.Overhead.EvictionThreshold.DeepCopy(),
			},
		}
	}), nil
}

func copyOfferings(offerings cloudprovider.Offerings) []*cloudprovider.Offering {
	return lo.Map(offerings, func(of *cloudprovider.Offering, _ int) *cloudprovider.Offering {
		return &cloudprovider.Offering{
			Requirements:        of.Requirements,
			Price:               of.Price,
			Available:           of.Available,
			ReservationCapacity: of.ReservationCapacity,
		}
	})
}

func addNodePoolRequirements(nodePool *v1.NodePool, its []*cloudprovider.InstanceType) {
	for _, it := range its {
		nodePoolReq := scheduling.NewRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpIn, nodePool.Name)
		nodeClassReq := scheduling.NewRequirement(v1.NodeClassLabelKey(nodePool.Spec.Template.Spec.NodeClassRef.GroupKind()), corev1.NodeSelectorOpIn, nodePool.Spec.Template.Spec.NodeClassRef.Name)
		it.Requirements.Add(scheduling.NewLabelRequirements(nodePool.Spec.Template.ObjectMeta.Labels).Values()...)
		it.Requirements.Add(nodePoolReq, nodeClassReq)
	}
}

func removeNodePoolRequirements(nodePool *v1.NodePool, its []*cloudprovider.InstanceType) {
	for _, it := range its {
		removeReq := []string{v1.NodePoolLabelKey, v1.NodeClassLabelKey(nodePool.Spec.Template.Spec.NodeClassRef.GroupKind())}
		removeReq = append(removeReq, lo.Keys(nodePool.Spec.Template.ObjectMeta.Labels)...)
		it.Requirements = lo.OmitByKeys(it.Requirements, removeReq)
	}
}
