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

package overlay

import (
	"context"
	"fmt"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay/validation"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type decorator struct {
	cloudprovider.CloudProvider
	kubeClient client.Client
	store      validation.InstanceTypeOverlayStore
}

// Decorate returns a new `CloudProvider` instance that will delegate the GetInstanceTypes
// calls to the argument, `cloudProvider`, and provide instance types with NodeOverlays applied to them. The
func Decorate(cloudProvider cloudprovider.CloudProvider, kubeClient client.Client, store validation.InstanceTypeOverlayStore) cloudprovider.CloudProvider {
	return &decorator{CloudProvider: cloudProvider, kubeClient: kubeClient, store: store}
}

func (d *decorator) GetInstanceTypes(ctx context.Context, nodePool *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	its, err := pullInstanceTypes(ctx, d.CloudProvider, nodePool)
	if err != nil {
		return []*cloudprovider.InstanceType{}, err
	}
	if options.FromContext(ctx).FeatureGates.NodeOverlay {
		its, err = d.OverlayInstanceTypes(ctx, d.kubeClient, its, nodePool)
		if err != nil {
			return []*cloudprovider.InstanceType{}, err
		}
	}
	return its, nil
}

func (d *decorator) OverlayInstanceTypes(ctx context.Context, kubeClient client.Client, instanceTypes []*cloudprovider.InstanceType, nodePool *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	overlays := &v1alpha1.NodeOverlayList{}
	err := kubeClient.List(ctx, overlays)
	if err != nil {
		return []*cloudprovider.InstanceType{}, err
	}

	// We order the overlays from largest to smallest, as we have already done validation
	// we can be sure that we will not be blocked due to invalid or conflicting overlays
	overlays.OrderByWeight()
	// Only apply overlays that have passed runtime validation and know there have not been
	// any updates to the spec of the overlay field
	validOverlays := lo.Filter(overlays.Items, func(o v1alpha1.NodeOverlay, _ int) bool {
		condition := o.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded)
		return condition.IsTrue() && condition.ObservedGeneration == o.Generation
	})
	d.applyOverlayOnInstanceTypes(nodePool, validOverlays, instanceTypes)

	return instanceTypes, nil
}

func (d *decorator) applyOverlayOnInstanceTypes(nodePool *v1.NodePool, overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType) {
	// we will only update instance types once
	overriddenPriceInstanceType := sets.Set[string]{}
	overriddenCapacityInstanceType := map[string][]string{}

	for _, overlay := range overlays {
		overlaySelector := scheduling.NewRequirements()
		overlaySelector.Add(scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...).Values()...)

		for _, it := range its {
			_, found := d.store.FindOverlaidInstanceTypes(*nodePool, overlay, it)
			if !found {
				continue
			}

			// Update the Capacity on the instance type
			for _, resource := range lo.Keys(overlay.Spec.Capacity) {
				if resourceList, found := overriddenCapacityInstanceType[it.Name]; !found || !lo.Contains(resourceList, resource.String()) {
					overriddenCapacityInstanceType[it.Name] = append(overriddenCapacityInstanceType[it.Name], resource.String())
					it.Capacity[resource] = overlay.Spec.Capacity[resource]
					it.ApplyResourceOverlay()
				}
			}

			// Update the Price on compatible offerings
			if overlay.Spec.Price != nil || overlay.Spec.PriceAdjustment != nil {
				for _, of := range it.Offerings.Compatible(overlaySelector) {
					offeringRequirementsHash := fmt.Sprint(lo.Must(hashstructure.Hash(fmt.Sprintf("%s-%s", it.Name, of.Requirements.String()), hashstructure.FormatV2, &hashstructure.HashOptions{})))
					if !overriddenPriceInstanceType.Has(offeringRequirementsHash) {
						overriddenPriceInstanceType.Insert(offeringRequirementsHash)
						of.Price = overlay.AdjustedPrice(of.Price)
						of.ApplyOverlay()
					}
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
