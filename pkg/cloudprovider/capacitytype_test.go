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
	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/labelstest"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

const onDemandAlt = "on-demand-alt"

func offering(capacityType string, price float64) *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available:  true,
		Compatible: true,
		Price:      price,
		Requirements: scheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:  capacityType,
			corev1.LabelTopologyZone: "test-zone-1",
		}),
	}
}

// optionsWithAlt returns core's label options with on-demand-alt as a known capacity type, as a provider that
// registers it must.
func optionsWithAlt() []labels.Option {
	opts := v1.LabelOptions()
	for i := range opts {
		if opts[i].Key == v1.CapacityTypeLabelKey {
			opts[i].KnownValues = append(opts[i].KnownValues, onDemandAlt)
		}
	}
	return opts
}

var _ = Describe("CapacityTypeClasses", func() {
	withAlt := cloudprovider.DefaultCapacityTypeClasses.With(cloudprovider.CapacityTypeClasses{onDemandAlt: cloudprovider.CapacityTypeClassOnDemand})
	useAlt := func() {
		labelstest.Register(GinkgoTB(), optionsWithAlt()...)
		Expect(cloudprovider.SetCapacityTypeClassResolver(withAlt)).To(Succeed())
	}

	// Other specs in the package build and cache the default state; start each spec from the unset state.
	BeforeEach(func() {
		test.ResetCapacityTypeClasses()
	})
	AfterEach(func() {
		test.ResetCapacityTypeClasses()
	})

	It("should resolve the built-in capacity types when no resolver was set", func() {
		for _, ct := range []string{v1.CapacityTypeReserved, v1.CapacityTypeSpot, v1.CapacityTypeOnDemand} {
			class, ok := cloudprovider.CapacityTypeClassOf(ct)
			Expect(ok).To(BeTrue())
			Expect(string(class)).To(Equal(ct))
		}
		_, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
		Expect(ok).To(BeFalse())
	})
	It("should fail on first use when an extra capacity type is a known value but no resolver was set", func() {
		labelstest.Register(GinkgoTB(), optionsWithAlt()...)
		Expect(func() { cloudprovider.CapacityTypeClassOf(v1.CapacityTypeSpot) }).To(PanicWith(ContainSubstring("must be the registered karpenter.sh/capacity-type known values")))
	})
	It("should register an additional on-demand class capacity type", func() {
		useAlt()
		class, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
		Expect(ok).To(BeTrue())
		Expect(class).To(Equal(cloudprovider.CapacityTypeClassOnDemand))
		Expect(cloudprovider.CapacityTypesOf(cloudprovider.CapacityTypeClassOnDemand)).To(Equal([]string{v1.CapacityTypeOnDemand, onDemandAlt}))
		Expect(cloudprovider.CapacityTypesOf(cloudprovider.CapacityTypeClassSpot)).To(Equal([]string{v1.CapacityTypeSpot}))
	})
	It("should restore the built-in capacity types on reset", func() {
		useAlt()
		// The built-in capacity types are checked against the registered known values on first use after the reset.
		labelstest.Register(GinkgoTB(), v1.LabelOptions()...)
		test.ResetCapacityTypeClasses()
		_, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
		Expect(ok).To(BeFalse())
	})
	DescribeTable("should reject invalid resolvers",
		func(classes cloudprovider.CapacityTypeClasses, message string) {
			Expect(cloudprovider.SetCapacityTypeClassResolver(classes)).To(MatchError(ContainSubstring(message)))
			_, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
			Expect(ok).To(BeFalse())
		},
		Entry("an additional capacity type outside the on-demand class", cloudprovider.DefaultCapacityTypeClasses.With(cloudprovider.CapacityTypeClasses{onDemandAlt: cloudprovider.CapacityTypeClassSpot}), `capacity type "on-demand-alt" must resolve to class "on-demand", got "spot"`),
		Entry("a built-in capacity type in another class", cloudprovider.DefaultCapacityTypeClasses.With(cloudprovider.CapacityTypeClasses{v1.CapacityTypeSpot: cloudprovider.CapacityTypeClassOnDemand, onDemandAlt: cloudprovider.CapacityTypeClassOnDemand}), `capacity type "spot" must resolve to class "spot"`),
		// Both built-in capacity types are missing; which one is reported depends on map order.
		Entry("a missing built-in capacity type", cloudprovider.CapacityTypeClasses{v1.CapacityTypeOnDemand: cloudprovider.CapacityTypeClassOnDemand, onDemandAlt: cloudprovider.CapacityTypeClassOnDemand}, `must resolve to class`),
	)
	It("should reject a resolver with a capacity type that isn't a registered known value", func() {
		Expect(cloudprovider.SetCapacityTypeClassResolver(withAlt)).To(MatchError(ContainSubstring("must be the registered karpenter.sh/capacity-type known values")))
		_, ok := cloudprovider.CapacityTypeClassOf(onDemandAlt)
		Expect(ok).To(BeFalse())
	})
	It("should reject a resolver without a registered known value", func() {
		labelstest.Register(GinkgoTB(), optionsWithAlt()...)
		Expect(cloudprovider.SetCapacityTypeClassResolver(cloudprovider.DefaultCapacityTypeClasses)).To(MatchError(ContainSubstring("must be the registered karpenter.sh/capacity-type known values")))
	})
	It("should report whether a requirement allows a class", func() {
		useAlt()
		alt := scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, onDemandAlt)
		Expect(cloudprovider.HasCapacityTypeClass(alt, cloudprovider.CapacityTypeClassOnDemand)).To(BeTrue())
		Expect(cloudprovider.HasCapacityTypeClass(alt, cloudprovider.CapacityTypeClassSpot)).To(BeFalse())
		notSpot := scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpNotIn, v1.CapacityTypeSpot)
		Expect(cloudprovider.HasCapacityTypeClass(notSpot, cloudprovider.CapacityTypeClassOnDemand)).To(BeTrue())
		Expect(cloudprovider.HasCapacityTypeClass(notSpot, cloudprovider.CapacityTypeClassSpot)).To(BeFalse())
	})
	Context("WorstLaunchPrice", func() {
		It("should price a requirement that only allows an additional capacity type by its offerings", func() {
			useAlt()
			ofs := cloudprovider.Offerings{offering(v1.CapacityTypeOnDemand, 1.0), offering(onDemandAlt, 0.8)}
			reqs := scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, onDemandAlt))
			Expect(ofs.WorstLaunchPrice(reqs)).To(Equal(0.8))
		})
		It("should price the on-demand class by its most expensive capacity type", func() {
			useAlt()
			ofs := cloudprovider.Offerings{offering(v1.CapacityTypeOnDemand, 1.0), offering(onDemandAlt, 0.8)}
			Expect(ofs.WorstLaunchPrice(scheduling.NewRequirements())).To(Equal(1.0))
		})
		It("should keep spot ahead of the on-demand class", func() {
			useAlt()
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
