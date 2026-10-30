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

package cloudprovider_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func offeringIn(zone, capacityType string, price float64) *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available: true,
		Price:     price,
		Requirements: scheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:  capacityType,
			corev1.LabelTopologyZone: zone,
		}),
	}
}

var _ = Describe("Offerings", func() {
	var ofs cloudprovider.Offerings
	BeforeEach(func() {
		ofs = cloudprovider.Offerings{
			offeringIn("zone-a", v1.CapacityTypeSpot, 0.3),
			offeringIn("zone-b", v1.CapacityTypeOnDemand, 1.0),
			offeringIn("zone-b", v1.CapacityTypeSpot, 0.5),
		}
	})

	Context("HasCompatible", func() {
		It("should report whether any offering satisfies the requirements", func() {
			inZone := func(zone string) scheduling.Requirements {
				return scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone))
			}
			Expect(ofs.HasCompatible(inZone("zone-a"))).To(BeTrue())
			Expect(ofs.HasCompatible(inZone("zone-c"))).To(BeFalse())
		})
		It("should require every requirement to match the same offering", func() {
			reqs := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"),
				scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
			)
			Expect(ofs.HasCompatible(reqs)).To(BeFalse(), "zone-a only has a spot offering")
		})
		It("should be true for empty requirements when there are offerings, and false when there are none", func() {
			Expect(ofs.HasCompatible(scheduling.NewRequirements())).To(BeTrue())
			Expect(cloudprovider.Offerings{}.HasCompatible(scheduling.NewRequirements())).To(BeFalse())
		})
		It("should agree with Compatible", func() {
			reqs := scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand))
			Expect(ofs.HasCompatible(reqs)).To(Equal(len(ofs.Compatible(reqs)) > 0))
		})
	})

	Context("MostExpensive and Cheapest", func() {
		It("should return the highest and lowest priced offering", func() {
			Expect(ofs.MostExpensive().Price).To(Equal(1.0))
			Expect(ofs.Cheapest().Price).To(Equal(0.3))
		})
		It("should return nil when there are no offerings", func() {
			Expect(cloudprovider.Offerings{}.MostExpensive()).To(BeNil())
			Expect(cloudprovider.Offerings{}.Cheapest()).To(BeNil())
		})
	})

	Context("InstanceType.OfferingPrice", func() {
		var it *cloudprovider.InstanceType
		BeforeEach(func() {
			it = &cloudprovider.InstanceType{Name: "test-type", Requirements: scheduling.NewRequirements(), Offerings: ofs}
		})
		It("should return the price of the offering in the zone and capacity type", func() {
			price, ok := it.OfferingPrice("zone-b", v1.CapacityTypeSpot)
			Expect(ok).To(BeTrue())
			Expect(price).To(Equal(0.5))
		})
		It("should report no price when no offering matches both the zone and the capacity type", func() {
			price, ok := it.OfferingPrice("zone-a", v1.CapacityTypeOnDemand)
			Expect(ok).To(BeFalse())
			Expect(price).To(BeZero())
			_, ok = it.OfferingPrice("zone-c", v1.CapacityTypeSpot)
			Expect(ok).To(BeFalse())
		})
	})

	Context("Offering.ApplyPriceOverlay", func() {
		It("should replace the price and mark the offering as overlaid", func() {
			o := offeringIn("zone-a", v1.CapacityTypeSpot, 2.0)
			Expect(o.IsPriceOverlaid()).To(BeFalse())
			o.ApplyPriceOverlay("1.5")
			Expect(o.Price).To(Equal(1.5))
			Expect(o.IsPriceOverlaid()).To(BeTrue())
		})
		It("should apply a percentage adjustment", func() {
			o := offeringIn("zone-a", v1.CapacityTypeSpot, 2.0)
			o.ApplyPriceOverlay("-50%")
			Expect(o.Price).To(Equal(1.0))
			Expect(o.IsPriceOverlaid()).To(BeTrue())
		})
		It("should leave the price alone for an empty overlay but still mark it overlaid", func() {
			o := offeringIn("zone-a", v1.CapacityTypeSpot, 2.0)
			o.ApplyPriceOverlay("")
			Expect(o.Price).To(Equal(2.0))
			Expect(o.IsPriceOverlaid()).To(BeTrue())
		})
		It("should make the instance type report that a pricing overlay is applied", func() {
			it := &cloudprovider.InstanceType{Name: "test-type", Requirements: scheduling.NewRequirements(), Offerings: ofs}
			Expect(it.IsPricingOverlayApplied()).To(BeFalse())
			ofs[1].ApplyPriceOverlay("+10%")
			Expect(it.IsPricingOverlayApplied()).To(BeTrue())
		})
	})
})
