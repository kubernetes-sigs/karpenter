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

package scheduling_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	pscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
)

var _ = Describe("NodeClaim", func() {
	var nodeClaim *scheduling.NodeClaim

	BeforeEach(func() {
		// Create instance types with different prices
		instanceTypes := []*cloudprovider.InstanceType{
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "cheapest",
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Offerings: []*cloudprovider.Offering{
					{
						Available: true,
						Requirements: pscheduling.NewLabelRequirements(map[string]string{
							v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
							corev1.LabelTopologyZone: "test-zone-1a",
						}),
						Price: 1.0,
					},
				},
			}),
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "middle",
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Offerings: []*cloudprovider.Offering{
					{
						Available: true,
						Requirements: pscheduling.NewLabelRequirements(map[string]string{
							v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
							corev1.LabelTopologyZone: "test-zone-1a",
						}),
						Price: 2.0,
					},
				},
			}),
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "expensive",
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Offerings: []*cloudprovider.Offering{
					{
						Available: true,
						Requirements: pscheduling.NewLabelRequirements(map[string]string{
							v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
							corev1.LabelTopologyZone: "test-zone-1a",
						}),
						Price: 4.0,
					},
				},
			}),
		}

		nodeClaim = &scheduling.NodeClaim{
			NodeClaimTemplate: scheduling.NodeClaimTemplate{
				InstanceTypeOptions: instanceTypes,
				Requirements: pscheduling.NewRequirements(
					pscheduling.NewRequirement("karpenter.sh/capacity-type", corev1.NodeSelectorOpIn, "on-demand"),
				),
			},
		}
	})

	It("should filter out instance types that don't meet the price improvement factor", func() {
		// Require 30% cost savings (price improvement factor of 0.7)
		maxPrice := 3.0
		priceImprovementFactor := 0.7

		result, err := nodeClaim.RemoveInstanceTypeOptionsByPriceAndMinValues(
			nodeClaim.Requirements, maxPrice, priceImprovementFactor)

		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypeOptions).To(HaveLen(2))
		// Both cheapest (1.0) and middle (2.0) should be less than 3.0 * 0.7 = 2.1
		names := lo.Map(result.InstanceTypeOptions, func(it *cloudprovider.InstanceType, _ int) string {
			return it.Name
		})
		Expect(names).To(ConsistOf("cheapest", "middle"))
	})

	It("should keep all instance types when price improvement factor is 1.0", func() {
		// Any cost savings is acceptable (legacy behavior)
		maxPrice := 3.0
		priceImprovementFactor := 1.0

		result, err := nodeClaim.RemoveInstanceTypeOptionsByPriceAndMinValues(
			nodeClaim.Requirements, maxPrice, priceImprovementFactor)

		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypeOptions).To(HaveLen(2))
		// Should keep cheapest (1.0) and middle (2.0) as both are less than 3.0
		names := lo.Map(result.InstanceTypeOptions, func(it *cloudprovider.InstanceType, _ int) string {
			return it.Name
		})
		Expect(names).To(ConsistOf("cheapest", "middle"))
	})

	It("should return empty instance types when no instances meet the price improvement factor", func() {
		// Require 50% cost savings (price improvement factor of 0.5)
		maxPrice := 1.2
		priceImprovementFactor := 0.5

		result, err := nodeClaim.RemoveInstanceTypeOptionsByPriceAndMinValues(
			nodeClaim.Requirements, maxPrice, priceImprovementFactor)

		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypeOptions).To(HaveLen(0))
		// No instances should be less than 1.2 * 0.5 = 0.6
	})

	It("should handle edge case of 0.0 price improvement factor", func() {
		// Disable consolidation by price (no price savings acceptable)
		maxPrice := 3.0
		priceImprovementFactor := 0.0

		result, err := nodeClaim.RemoveInstanceTypeOptionsByPriceAndMinValues(
			nodeClaim.Requirements, maxPrice, priceImprovementFactor)

		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypeOptions).To(HaveLen(0))
		// No instances should be less than 3.0 * 0.0 = 0.0
	})

	It("should validate min values after price filtering", func() {
		// Add a minValues requirement that might not be satisfiable after filtering
		nodeClaim.Requirements.Add(pscheduling.NewRequirement("instance-type", corev1.NodeSelectorOpIn, "cheapest", "middle"))
		req := nodeClaim.Requirements.Get("instance-type")
		req.MinValues = lo.ToPtr(2) // Require at least 2 instance types

		maxPrice := 3.0
		priceImprovementFactor := 0.7 // Only cheapest will pass

		_, err := nodeClaim.RemoveInstanceTypeOptionsByPriceAndMinValues(
			nodeClaim.Requirements, maxPrice, priceImprovementFactor)

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("minValues requirement"))
	})

	It("should handle very small price improvement factor", func() {
		// Very aggressive cost savings requirement
		maxPrice := 4.0
		priceImprovementFactor := 0.1 // Require 90% cost savings

		result, err := nodeClaim.RemoveInstanceTypeOptionsByPriceAndMinValues(
			nodeClaim.Requirements, maxPrice, priceImprovementFactor)

		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypeOptions).To(HaveLen(0))
		// No instances should be less than 4.0 * 0.1 = 0.4
	})

	DescribeTable("should handle price improvement factor edge cases",
		func(maxPrice float64, priceImprovementFactor float64, expectedInstanceCount int) {
			result, err := nodeClaim.RemoveInstanceTypeOptionsByPriceAndMinValues(
				nodeClaim.Requirements, maxPrice, priceImprovementFactor)

			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypeOptions).To(HaveLen(expectedInstanceCount))
		},
		Entry("no cost savings allowed", 3.0, 0.0, 0),
		Entry("moderate cost savings required", 3.0, 0.8, 2),
		Entry("any cost savings allowed", 3.0, 1.0, 2),
	)
})
