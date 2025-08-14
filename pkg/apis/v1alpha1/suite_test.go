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

package v1alpha1_test

import (
	"context"
	"math/rand/v2"
	"testing"

	. "github.com/awslabs/operatorpkg/test/expectations"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/test"
	testexpectations "sigs.k8s.io/karpenter/pkg/test/expectations"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "v1alpha1")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(testv1alpha1.CRDs...))
})

var _ = AfterEach(func() {
	testexpectations.ExpectCleanedUp(ctx, env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("NodeOverlay", func() {
	Context("OrderByWeight", func() {
		It("should order the NodeOverlay by weight", func() {
			// Generate 10 NodeOverlay that have random weights, some might have the same weights
			nos := lo.Times(10, func(_ int) *v1alpha1.NodeOverlay {
				return test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Weight: lo.ToPtr[int32](int32(rand.IntN(100) + 1)), //nolint:gosec
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      "test",
								Operator: corev1.NodeSelectorOpExists,
							},
						},
					},
				})
			})
			lo.ForEach(nos, func(overlay *v1alpha1.NodeOverlay, _ int) {
				ExpectApplied(ctx, env.Client, overlay)
			})
			overlayList := &v1alpha1.NodeOverlayList{}
			Expect(env.Client.List(ctx, overlayList)).To(BeNil())
			overlayList.OrderByWeight()

			lastWeight := 101 // This is above the allowed weight values
			for _, overlay := range overlayList.Items {
				Expect(lo.FromPtr(overlay.Spec.Weight)).To(BeNumerically("<=", lastWeight))
				lastWeight = int(lo.FromPtr(overlay.Spec.Weight))
			}
		})
		It("should order the NodeOverlay by name when the weights are the same", func() {
			// Generate 10 NodePools with the same weight
			nos := lo.Times(10, func(_ int) *v1alpha1.NodeOverlay {
				return test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Weight: lo.ToPtr[int32](10),
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      "test",
								Operator: corev1.NodeSelectorOpExists,
							},
						},
					},
				})
			})
			lo.ForEach(nos, func(overlay *v1alpha1.NodeOverlay, _ int) {
				ExpectApplied(ctx, env.Client, overlay)
			})
			overlayList := &v1alpha1.NodeOverlayList{}
			Expect(env.Client.List(ctx, overlayList)).To(BeNil())
			overlayList.OrderByWeight()

			lastName := "zzzzzzzzzzzzzzzzzzzzzzzz" // large string value
			for _, overlay := range overlayList.Items {
				Expect(overlay.Name < lastName).To(BeTrue())
				lastName = overlay.Name
			}
		})
	})
	Context("AdjustedPrice", func() {
		DescribeTable("should adjust price based overlay values",
			func(priceAdjustment string, basePrice float64, expectedPrice float64) {
				overlay := &v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						PriceAdjustment: lo.ToPtr(priceAdjustment),
					},
				}
				adjustedPrice := overlay.AdjustedPrice(basePrice)
				Expect(adjustedPrice).To(BeNumerically("==", expectedPrice))
			},
			// Percentage adjustment
			Entry("No change", "0%", 10.0, 10.0),
			Entry("10% decrease", "-10%", 10.0, 9.0),
			Entry("10% increase", "+10%", 10.0, 11.0),
			Entry("50% decrease", "-50%", 10.0, 5.0),
			Entry("100% increase", "+100%", 10.0, 20.0),
			Entry("Zero price", "-100%", 10.0, 0.0),
			Entry("Zero price", "-200%", 10.0, 0.0),
			Entry("Fractional price", "-25%", 1.5, 1.125),
			// Raw adjustment
			Entry("No change", "+0", 10.0, 10.0),
			Entry("Add 5", "+5", 10.0, 15.0),
			Entry("Subtract 2.5", "-2.5", 10.0, 7.5),
			Entry("Subtract to zero", "-10", 10.0, 0.0),
			Entry("Negative Result", "-15", 10.0, 0.0),
			Entry("Fractional price", "+0.75", 1.25, 2.0),
			Entry("Large price adjustment", "+100", 0.001, 100.001),
			Entry("Small price adjustment", "+0.0001", 0.0001, 0.0002),
		)
		It("should override price", func() {
			overlay := &v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Price: lo.ToPtr("80.0"),
				},
			}
			adjustedPrice := overlay.AdjustedPrice(82781.0)
			Expect(adjustedPrice).To(BeNumerically("==", 80))
		})
		It("should provide the same if price or priceAdjustment is not provided", func() {
			overlay := &v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{},
			}
			adjustedPrice := overlay.AdjustedPrice(82781.0)
			Expect(adjustedPrice).To(BeNumerically("==", 82781))
		})
	})
})
