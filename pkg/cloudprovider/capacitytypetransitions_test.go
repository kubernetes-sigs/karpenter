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

var _ = Describe("Capacity-type transitions", func() {
	var offering *cloudprovider.Offering
	BeforeEach(func() {
		offering = onDemandOffering(1.0)
		offering.CapacityTypeTransitions = []string{v1.CapacityTypeReserved}
	})
	DescribeTable("requires compatibility with every possible capacity type", func(operator corev1.NodeSelectorOperator, values []string, compatible bool) {
		reqs := scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, operator, values...))
		Expect(offering.IsCompatible(reqs)).To(Equal(compatible))
		Expect(cloudprovider.Offerings{offering}.HasCompatible(reqs)).To(Equal(compatible))
		Expect(cloudprovider.Offerings{offering}.Compatible(reqs)).To(HaveLen(map[bool]int{true: 1, false: 0}[compatible]))
	},
		Entry("both states", corev1.NodeSelectorOpIn, []string{v1.CapacityTypeOnDemand, v1.CapacityTypeReserved}, true),
		Entry("On-Demand only", corev1.NodeSelectorOpIn, []string{v1.CapacityTypeOnDemand}, false),
		Entry("reserved only", corev1.NodeSelectorOpIn, []string{v1.CapacityTypeReserved}, false),
		Entry("Spot only", corev1.NodeSelectorOpIn, []string{v1.CapacityTypeSpot}, false),
		Entry("excludes reserved", corev1.NodeSelectorOpNotIn, []string{v1.CapacityTypeReserved}, false),
		Entry("excludes Spot", corev1.NodeSelectorOpNotIn, []string{v1.CapacityTypeSpot}, true),
		Entry("exists", corev1.NodeSelectorOpExists, nil, true),
	)
	It("preserves launch market, price, and zero reservation capacity", func() {
		Expect(offering.CapacityType()).To(Equal(v1.CapacityTypeOnDemand))
		Expect(offering.CapacityTypes()).To(ConsistOf(v1.CapacityTypeOnDemand, v1.CapacityTypeReserved))
		Expect(offering.Launchable()).To(BeTrue())
		Expect(offering.ReservationCapacity).To(BeZero())
		Expect(cloudprovider.Offerings{offering}.WorstLaunchPrice(scheduling.NewRequirements())).To(Equal(1.0))
	})
	It("does not mutate the source transition slice when deep copied", func() {
		copy := offering.DeepCopy()
		copy.CapacityTypeTransitions[0] = v1.CapacityTypeSpot
		Expect(offering.CapacityTypeTransitions).To(Equal([]string{v1.CapacityTypeReserved}))
	})
	It("preserves ordinary On-Demand behavior", func() {
		offering.CapacityTypeTransitions = nil
		Expect(offering.IsCompatible(cloudprovider.OnDemandRequirement)).To(BeTrue())
		Expect(offering.CapacityTypes()).To(Equal([]string{v1.CapacityTypeOnDemand}))
	})
	It("does not price an incompatible mutable offering as a launch option", func() {
		mutable := &cloudprovider.InstanceType{Name: "mutable", Offerings: cloudprovider.Offerings{offering}}
		ordinary := &cloudprovider.InstanceType{Name: "ordinary", Offerings: cloudprovider.Offerings{onDemandOffering(2.0)}}
		ordered := cloudprovider.InstanceTypes{mutable, ordinary}.OrderByPrice(cloudprovider.OnDemandRequirement)
		Expect(ordered[0].Name).To(Equal("ordinary"))
	})
})
