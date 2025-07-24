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
	overlay := &v1alpha1.NodeOverlayList{}
	err = kubeClient.List(ctx, overlay)
	if err != nil {
		return []*cloudprovider.InstanceType{}, err
	}

	return OverlayInstanceTypes(overlay.Items, its), nil
}

func OverlayInstanceTypes(overlays []v1alpha1.NodeOverlay, instanceTypes []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	// Only apply overlays that have passed runtime validation and know there have not been
	// any updates to the spec of the overlay field
	validOverlays := lo.Filter(overlays, func(o v1alpha1.NodeOverlay, _ int) bool {
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

	return instanceTypes
}

func overlayCapacityOnInstanceTypes(overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType) {
	for _, overlay := range overlays {
		overlaySelector := scheduling.NewRequirements()
		overlaySelector.Add(scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...).Values()...)

		for _, it := range its {
			if it.Requirements.Intersects(overlaySelector) == nil {
				it.Capacity = lo.Assign(it.Capacity, overlay.Spec.Capacity)
			}
		}
	}
}

func overlayPriceOnInstanceTypes(overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType) {
	overriddenInstanceType := map[string][]string{}
	for _, overlay := range overlays {
		// if price or price adjustment is not defined, then we should skip the overlay
		if overlay.Spec.Price == nil && overlay.Spec.PriceAdjustment == nil {
			continue
		}
		overlaySelector := scheduling.NewRequirements()
		overlaySelector.Add(scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...).Values()...)

		for _, it := range its {
			if err := it.Requirements.Intersects(overlaySelector); err != nil {
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
		return &cloudprovider.InstanceType{
			Name:         it.Name,
			Requirements: it.Requirements,
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
