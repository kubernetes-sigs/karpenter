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
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func TestPriceOverlayPreservesMutableOffering(t *testing.T) {
	original := &cloudprovider.Offering{Price: 1, Available: true,
		CapacityTypeTransitions: []string{v1.CapacityTypeReserved},
		Requirements:            scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand)),
		CapacityOverride:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
	}
	store := newInternalInstanceTypeStore()
	actual := store.applyPriceOverlays(cloudprovider.Offerings{original}, map[string]*priceUpdate{original.Requirements.String(): {OverlayUpdate: lo.ToPtr("-50%")}})[0]
	strict := scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand))
	broad := scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand, v1.CapacityTypeReserved))
	// The controller must still select the actual OD launch market for an
	// OD-only price overlay even though OD-only workloads cannot use it.
	it := &cloudprovider.InstanceType{Requirements: broad, Offerings: cloudprovider.Offerings{original}}
	if len(getOverlaidOfferings(v1.NodePool{}, it, strict)) != 1 {
		t.Fatal("OD price overlay did not select mutable OD launch offering")
	}
	if (cloudprovider.Offerings{actual}).HasCompatible(strict) || !(cloudprovider.Offerings{actual}).HasCompatible(broad) {
		t.Fatal("price overlay changed workload compatibility")
	}
	if len(actual.CapacityTypeTransitions) != 1 || len(actual.CapacityOverride) != 1 || actual.Price != .5 || original.Price != 1 {
		t.Fatalf("lost offering metadata or mutated original: %+v", actual)
	}
}

func TestObservedNodeOverlayPricing(t *testing.T) {
	store := NewInstanceTypeStore()
	internal := newInternalInstanceTypeStore()
	internal.evaluatedNodePools.Insert("pool")
	internal.priceOverlays = []v1alpha1.NodeOverlay{{Spec: v1alpha1.NodeOverlaySpec{Weight: lo.ToPtr[int32](10), Price: lo.ToPtr("0.25"), Requirements: []v1alpha1.NodeSelectorRequirement{{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeReserved}}}}}}
	store.UpdateStore(internal)
	for capacityType, want := range map[string]float64{v1.CapacityTypeReserved: .25, v1.CapacityTypeOnDemand: 1} {
		got, err := store.ApplyNodeClaimPrice(map[string]string{v1.NodePoolLabelKey: "pool", v1.CapacityTypeLabelKey: capacityType}, 1)
		if err != nil || got != want {
			t.Fatalf("%s price = %v, %v; want %v", capacityType, got, err, want)
		}
	}
	internal.priceOverlays = append(internal.priceOverlays, internal.priceOverlays[0])
	if _, err := store.ApplyNodeClaimPrice(map[string]string{v1.NodePoolLabelKey: "pool", v1.CapacityTypeLabelKey: v1.CapacityTypeReserved}, 0); err == nil {
		t.Fatal("expected conflicting observed overlays to defer pricing")
	}
}
