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
	"context"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

const onDemandAlt = "on-demand-alt"

func offering(capacityType string, price float64) *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available: true,
		Price:     price,
		Requirements: scheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:  capacityType,
			corev1.LabelTopologyZone: "test-zone-1",
		}),
	}
}

var _ = Describe("CapacityTypeClasses", func() {
	withAlt := cloudprovider.DefaultCapacityTypeClasses.With(cloudprovider.CapacityTypeClasses{onDemandAlt: cloudprovider.CapacityTypeClassOnDemand})

	AfterEach(func() {
		test.ResetCapacityTypeClasses()
	})

	It("should resolve the built-in capacity types by default", func() {
		for _, ct := range []string{v1.CapacityTypeReserved, v1.CapacityTypeSpot, v1.CapacityTypeOnDemand} {
			class, ok := cloudprovider.CapacityTypeClassOf(ct)
			Expect(ok).To(BeTrue())
			Expect(string(class)).To(Equal(ct))
		}
		_, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
		Expect(ok).To(BeFalse())
		Expect(v1.WellKnownValuesForRequirements[v1.CapacityTypeLabelKey].UnsortedList()).To(ConsistOf(v1.CapacityTypeReserved, v1.CapacityTypeSpot, v1.CapacityTypeOnDemand))
	})
	It("should register an additional on-demand class capacity type", func() {
		Expect(cloudprovider.SetCapacityTypeClassResolver(withAlt)).To(Succeed())
		class, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
		Expect(ok).To(BeTrue())
		Expect(class).To(Equal(cloudprovider.CapacityTypeClassOnDemand))
		Expect(cloudprovider.CapacityTypesOf(cloudprovider.CapacityTypeClassOnDemand)).To(Equal([]string{v1.CapacityTypeOnDemand, onDemandAlt}))
		Expect(cloudprovider.CapacityTypesOf(cloudprovider.CapacityTypeClassSpot)).To(Equal([]string{v1.CapacityTypeSpot}))
		Expect(v1.WellKnownValuesForRequirements[v1.CapacityTypeLabelKey].Has(onDemandAlt)).To(BeTrue())
	})
	It("should restore the built-in capacity types on reset", func() {
		Expect(cloudprovider.SetCapacityTypeClassResolver(withAlt)).To(Succeed())
		test.ResetCapacityTypeClasses()
		_, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
		Expect(ok).To(BeFalse())
		Expect(v1.WellKnownValuesForRequirements[v1.CapacityTypeLabelKey].Has(onDemandAlt)).To(BeFalse())
	})
	DescribeTable("should reject invalid resolvers",
		func(classes cloudprovider.CapacityTypeClasses) {
			Expect(cloudprovider.SetCapacityTypeClassResolver(classes)).ToNot(Succeed())
			_, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
			Expect(ok).To(BeFalse())
		},
		Entry("an additional capacity type outside the on-demand class", cloudprovider.DefaultCapacityTypeClasses.With(cloudprovider.CapacityTypeClasses{onDemandAlt: cloudprovider.CapacityTypeClassSpot})),
		Entry("a built-in capacity type in another class", cloudprovider.DefaultCapacityTypeClasses.With(cloudprovider.CapacityTypeClasses{v1.CapacityTypeSpot: cloudprovider.CapacityTypeClassOnDemand, onDemandAlt: cloudprovider.CapacityTypeClassOnDemand})),
		Entry("a missing built-in capacity type", cloudprovider.CapacityTypeClasses{v1.CapacityTypeOnDemand: cloudprovider.CapacityTypeClassOnDemand, onDemandAlt: cloudprovider.CapacityTypeClassOnDemand}),
	)
	It("should report whether a requirement allows a class", func() {
		Expect(cloudprovider.SetCapacityTypeClassResolver(withAlt)).To(Succeed())
		alt := scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, onDemandAlt)
		Expect(cloudprovider.HasCapacityTypeClass(alt, cloudprovider.CapacityTypeClassOnDemand)).To(BeTrue())
		Expect(cloudprovider.HasCapacityTypeClass(alt, cloudprovider.CapacityTypeClassSpot)).To(BeFalse())
		notSpot := scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpNotIn, v1.CapacityTypeSpot)
		Expect(cloudprovider.HasCapacityTypeClass(notSpot, cloudprovider.CapacityTypeClassOnDemand)).To(BeTrue())
		Expect(cloudprovider.HasCapacityTypeClass(notSpot, cloudprovider.CapacityTypeClassSpot)).To(BeFalse())
	})
	Context("WorstLaunchPrice", func() {
		It("should price a requirement that only allows an additional capacity type by its offerings", func() {
			Expect(cloudprovider.SetCapacityTypeClassResolver(withAlt)).To(Succeed())
			ofs := cloudprovider.Offerings{offering(v1.CapacityTypeOnDemand, 1.0), offering(onDemandAlt, 0.8)}
			reqs := scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, onDemandAlt))
			Expect(ofs.WorstLaunchPrice(reqs)).To(Equal(0.8))
		})
		It("should price the on-demand class by its most expensive capacity type", func() {
			Expect(cloudprovider.SetCapacityTypeClassResolver(withAlt)).To(Succeed())
			ofs := cloudprovider.Offerings{offering(v1.CapacityTypeOnDemand, 1.0), offering(onDemandAlt, 0.8)}
			Expect(ofs.WorstLaunchPrice(scheduling.NewRequirements())).To(Equal(1.0))
		})
		It("should keep spot ahead of the on-demand class", func() {
			Expect(cloudprovider.SetCapacityTypeClassResolver(withAlt)).To(Succeed())
			ofs := cloudprovider.Offerings{offering(v1.CapacityTypeSpot, 0.3), offering(onDemandAlt, 0.8)}
			Expect(ofs.WorstLaunchPrice(scheduling.NewRequirements())).To(Equal(0.3))
		})
	})
})

type pricer struct {
	price float64
	ok    bool
}

func (p pricer) NodeClaimPrice(_ context.Context, _ *v1.NodeClaim, _ *cloudprovider.InstanceType) (float64, bool) {
	return p.price, p.ok
}

var _ = Describe("PriceNodeClaim", func() {
	ctx := context.Background()
	var nodeClaim *v1.NodeClaim
	var instanceType *cloudprovider.InstanceType

	BeforeEach(func() {
		nodeClaim = test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
			corev1.LabelTopologyZone: "test-zone-1",
		}}})
		instanceType = &cloudprovider.InstanceType{Name: "test", Offerings: cloudprovider.Offerings{offering(v1.CapacityTypeOnDemand, 1.0), offering(v1.CapacityTypeSpot, 0.3)}}
		DeferCleanup(test.ResetNodeClaimPriceProvider)
	})

	It("should price by the NodeClaim's offering by default", func() {
		price, ok := cloudprovider.PriceNodeClaim(ctx, nodeClaim, instanceType)
		Expect(ok).To(BeTrue())
		Expect(price).To(Equal(1.0))
	})
	It("should be unknown without an instance type or a matching offering", func() {
		_, ok := cloudprovider.PriceNodeClaim(ctx, nodeClaim, nil)
		Expect(ok).To(BeFalse())
		nodeClaim.Labels[corev1.LabelTopologyZone] = "test-zone-2"
		_, ok = cloudprovider.PriceNodeClaim(ctx, nodeClaim, instanceType)
		Expect(ok).To(BeFalse())
	})
	It("should use the registered NodeClaimPriceProvider", func() {
		cloudprovider.SetNodeClaimPriceProvider(pricer{price: 0.5, ok: true})
		Expect(cloudprovider.GetNodeClaimPriceProvider()).To(Equal(pricer{price: 0.5, ok: true}))
		price, ok := cloudprovider.PriceNodeClaim(ctx, nodeClaim, instanceType)
		Expect(ok).To(BeTrue())
		Expect(price).To(Equal(0.5))
	})
	DescribeTable("should treat invalid prices as unknown",
		func(price float64) {
			cloudprovider.SetNodeClaimPriceProvider(pricer{price: price, ok: true})
			_, ok := cloudprovider.PriceNodeClaim(ctx, nodeClaim, instanceType)
			Expect(ok).To(BeFalse())
		},
		Entry("NaN", math.NaN()),
		Entry("+Inf", math.Inf(1)),
		Entry("-Inf", math.Inf(-1)),
		Entry("negative", -0.1),
	)
	It("should restore the default when set to nil", func() {
		cloudprovider.SetNodeClaimPriceProvider(pricer{price: 0.5, ok: true})
		cloudprovider.SetNodeClaimPriceProvider(nil)
		Expect(cloudprovider.GetNodeClaimPriceProvider()).To(Equal(cloudprovider.DefaultNodeClaimPriceProvider{}))
	})
})
