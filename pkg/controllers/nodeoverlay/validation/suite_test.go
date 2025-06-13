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

package validation_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay/validation"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx                             context.Context
	env                             *test.Environment
	nodeOverlayValidationController *validation.Controller
)

func TestValidation(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodeOverlay Status")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(testv1alpha1.CRDs...), test.WithFieldIndexers(test.NodeOverlayRefFieldIndexer(ctx)))
	nodeOverlayValidationController = validation.NewController(env.Client)
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Validation", func() {
	It("should pass with a single overlay", func() {
		overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
			Spec: v1alpha1.NodeOverlaySpec{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      "instance-type",
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"m5.large"},
					},
				},
				Weight: lo.ToPtr(int64(10)),
			},
		})
		ExpectApplied(ctx, env.Client, overlay)
		ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlay)

		// Check that the condition was set correctly
		updatedOverlay := ExpectExists(ctx, env.Client, overlay)
		Expect(updatedOverlay.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
	})
	Context("Pricing adjustment", func() {
		It("should fail with conflicting pricing overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.xlarge"},
						},
					},
					Weight:          lo.ToPtr(int64(10)),
					PriceAdjustment: "54",
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.2xlarge"},
						},
					},
					Weight:          lo.ToPtr(int64(10)),
					PriceAdjustment: "2",
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayB.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))
		})
		It("should pass with pricing adjustment are the same overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.xlarge"},
						},
					},
					Weight:          lo.ToPtr(int64(10)),
					PriceAdjustment: "100",
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.2xlarge"},
						},
					},
					Weight:          lo.ToPtr(int64(10)),
					PriceAdjustment: "100",
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting pricing overlays with mutually exclusive requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large"},
						},
					},
					Weight:          lo.ToPtr(int64(10)),
					PriceAdjustment: "54",
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpNotIn,
							Values:   []string{"m5.large"},
						},
					},
					Weight:          lo.ToPtr(int64(10)),
					PriceAdjustment: "2",
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting pricing overlays with mutually exclusive weights", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large"},
						},
					},
					Weight:          lo.ToPtr(int64(10)),
					PriceAdjustment: "54",
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.2xlarge"},
						},
					},
					Weight:          lo.ToPtr(int64(20)),
					PriceAdjustment: "2",
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
	})
	Context("Capacity Adjustment", func() {
		It("should fail with conflicting capacity overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.xlarge"},
						},
					},
					Weight: lo.ToPtr(int64(10)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.2xlarge"},
						},
					},
					Weight: lo.ToPtr(int64(10)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("54"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayB.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))
		})
		It("should pass with capacity adjustment are the same overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.xlarge"},
						},
					},
					Weight: lo.ToPtr(int64(10)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("54"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.2xlarge"},
						},
					},
					Weight: lo.ToPtr(int64(10)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("54"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting capacity overlays with mutually exclusive requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large"},
						},
					},
					Weight: lo.ToPtr(int64(10)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("54"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpNotIn,
							Values:   []string{"m5.large"},
						},
					},
					Weight: lo.ToPtr(int64(10)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("5"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting capacity overlays with mutually exclusive weights", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large"},
						},
					},
					Weight: lo.ToPtr(int64(10)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("55"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.2xlarge"},
						},
					},
					Weight: lo.ToPtr(int64(20)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("5"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with non conflicting capacity overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large"},
						},
					},
					Weight: lo.ToPtr(int64(10)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/fuse"): resource.MustParse("55"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      "instance-type",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"m5.large", "m5.2xlarge"},
						},
					},
					Weight: lo.ToPtr(int64(20)),
					Capacity: v1.ResourceList{
						v1.ResourceName("smarter-devices/buz"): resource.MustParse("5"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayA)
			ExpectObjectReconciled(ctx, env.Client, nodeOverlayValidationController, overlayB)

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
	})
})
