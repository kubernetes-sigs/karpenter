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

// These specs pin down that offering launchability accounts for reservation capacity, not just Available — so a
// full-but-healthy reservation (Available=true, ReservationCapacity=0) is not treated as launchable/cheapest. This is
// the invariant the pricing/ordering call sites rely on after capacity and availability were decoupled.

func reservedOffering(available bool, capacity int) *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available:           available,
		ReservationCapacity: capacity,
		Price:               0.001,
		Requirements: scheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:  v1.CapacityTypeReserved,
			corev1.LabelTopologyZone: "test-zone-1",
		}),
	}
}

func onDemandOffering(price float64) *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available: true,
		Price:     price,
		Requirements: scheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
			corev1.LabelTopologyZone: "test-zone-1",
		}),
	}
}

var _ = Describe("Offering Launchability", func() {
	It("Launchable requires reservation capacity for reserved offerings", func() {
		Expect(reservedOffering(true, 0).Launchable()).To(BeFalse(), "full reservation is Available but not launchable")
		Expect(reservedOffering(true, 3).Launchable()).To(BeTrue(), "reservation with capacity is launchable")
		Expect(reservedOffering(false, 3).Launchable()).To(BeFalse(), "unavailable reservation is not launchable even with capacity")
	})
	It("Launchable equals Available for non-reserved offerings", func() {
		Expect(onDemandOffering(1.0).Launchable()).To(BeTrue())
		od := onDemandOffering(1.0)
		od.Available = false
		Expect(od.Launchable()).To(BeFalse())
	})
	It("Offerings.Launchable filters out full reservations but keeps on-demand", func() {
		ofs := cloudprovider.Offerings{reservedOffering(true, 0), onDemandOffering(1.0)}
		launchable := ofs.Launchable()
		Expect(launchable).To(HaveLen(1))
		Expect(launchable[0].CapacityType()).To(Equal(v1.CapacityTypeOnDemand))
	})

	It("WorstLaunchPrice off Launchable ignores a full reservation's near-zero price", func() {
		ofs := cloudprovider.Offerings{reservedOffering(true, 0), onDemandOffering(1.0)}
		reqs := scheduling.NewRequirements()
		// Availability alone would price this as the ~0 reserved offering (the bug we are guarding against)...
		Expect(ofs.Available().WorstLaunchPrice(reqs)).To(BeNumerically("~", 0.001))
		// ...but the node will really launch on-demand, which Launchable correctly reflects.
		Expect(ofs.Launchable().WorstLaunchPrice(reqs)).To(Equal(1.0))
	})

	It("OrderByPrice does not rank an instance type cheap on a full reservation it can't launch into", func() {
		phantom := &cloudprovider.InstanceType{
			Name:         "phantom-cheap",
			Requirements: scheduling.NewRequirements(),
			// A full reservation (~0, not launchable) plus a genuine on-demand price of 2.0.
			Offerings: cloudprovider.Offerings{reservedOffering(true, 0), onDemandOffering(2.0)},
		}
		genuine := &cloudprovider.InstanceType{
			Name:         "genuine-cheap",
			Requirements: scheduling.NewRequirements(),
			Offerings:    cloudprovider.Offerings{onDemandOffering(1.0)},
		}
		ordered := cloudprovider.InstanceTypes{phantom, genuine}.OrderByPrice(scheduling.NewRequirements())
		// genuine (launchable 1.0) must sort before phantom (launchable 2.0); the phantom's ~0 reserved price is ignored.
		Expect(ordered[0].Name).To(Equal("genuine-cheap"))
		Expect(ordered[1].Name).To(Equal("phantom-cheap"))
	})
})
