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

package nodeoverlay_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx                   context.Context
	env                   *test.Environment
	cloudProvider         *fake.CloudProvider
	nodePool              *v1.NodePool
	nodeOverlayController *nodeoverlay.Controller
	store                 *nodeoverlay.InstanceTypeStore
)

func TestNodeOverlay(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodeOverlay Status")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(testv1alpha1.CRDs...))
	cloudProvider = fake.NewCloudProvider()
	store = nodeoverlay.NewInstanceTypeStore()
	nodeOverlayController = nodeoverlay.NewController(env.Client, cloudProvider, store)
})

var _ = BeforeEach(func() {
	nodePool = test.NodePool()
	cloudProvider.Reset()
	store.Reset()

	ExpectApplied(ctx, env.Client, nodePool)
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
				Requirements: []corev1.NodeSelectorRequirement{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"default-instance-type"},
					},
				},
				Weight: lo.ToPtr(int32(10)),
			},
		})
		ExpectApplied(ctx, env.Client, overlay)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		// Check that the condition was set correctly
		updatedOverlay := ExpectExists(ctx, env.Client, overlay)
		Expect(updatedOverlay.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
	})
	Context("Runtime Validation", func() {
		It("should fail validation for invalid requirements values", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      fmt.Sprintf("test.com.test/test-%s", strings.ToLower(randomdata.Alphanumeric(250))),
							Operator: corev1.NodeSelectorOpExists,
						},
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})

			ExpectApplied(ctx, env.Client, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			updatedOverlay := ExpectExists(ctx, env.Client, overlay)
			Expect(updatedOverlay.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlay.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("RuntimeValidation"))
		})
		It("should fail validation for invalid capacity values", func() {
			v1.WellKnownResources.Insert(corev1.ResourceName("testResource"))
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("testResource"): resource.MustParse("5"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})

			ExpectApplied(ctx, env.Client, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			updatedOverlay := ExpectExists(ctx, env.Client, overlay)
			Expect(updatedOverlay.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlay.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("RuntimeValidation"))
		})
	})
	Context("Requirements Validations", func() {
		Describe("Instance types Requirements", func() {
			It("should fail when requirements overlays overlap", func() {
				overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-a",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      corev1.LabelArchStable,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"arm64"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("1.03"),
					},
				})
				overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-b",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.CapacityTypeLabelKey,
								Operator: corev1.NodeSelectorOpExists,
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("23"),
					},
				})
				ExpectApplied(ctx, env.Client, overlayA, overlayB)

				// Reconcile both overlays
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				// Check that the conditions were set correctly
				updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
				Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
				Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

				updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
				Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			})
			It("should succeed with requirements overlays don't overlap", func() {
				cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
					fake.NewInstanceType(fake.InstanceTypeOptions{
						Name:             "arm-instance-type",
						Architecture:     "arm64",
						OperatingSystems: sets.New(string(corev1.Linux), string(corev1.Windows), "darwin"),
					}),
					fake.NewInstanceType(fake.InstanceTypeOptions{
						Name:             "amd-instance-type",
						Architecture:     "amd64",
						OperatingSystems: sets.New("ios"),
					}),
				}
				overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-a",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      corev1.LabelArchStable,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"arm64"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("1.03"),
					},
				})
				overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-b",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      corev1.LabelOSStable,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"ios"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("23"),
					},
				})
				ExpectApplied(ctx, env.Client, overlayA, overlayB)

				// Reconcile both overlays
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				// Check that the conditions were set correctly
				updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
				Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

				updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
				Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
			})
		})
		Describe("Offering Requirements", func() {
			BeforeEach(func() {
				cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
					fake.NewInstanceType(fake.InstanceTypeOptions{
						Name: "default-instance-type",
						Offerings: []*cloudprovider.Offering{
							{
								Available: true,
								Requirements: scheduling.NewLabelRequirements(map[string]string{
									v1.CapacityTypeLabelKey:  "spot",
									corev1.LabelTopologyZone: "test-zone-1",
								}),
								Price: 1.020,
							},
							{
								Available: true,
								Requirements: scheduling.NewLabelRequirements(map[string]string{
									v1.CapacityTypeLabelKey:  "on-demand",
									corev1.LabelTopologyZone: "test-zone-2",
								}),
								Price: 2.020,
							},
							{
								Available: true,
								Requirements: scheduling.NewLabelRequirements(map[string]string{
									v1.CapacityTypeLabelKey:  "spot",
									corev1.LabelTopologyZone: "test-zone-3",
								}),
								Price: 3.020,
							},
							{
								Available: true,
								Requirements: scheduling.NewLabelRequirements(map[string]string{
									v1.CapacityTypeLabelKey:  "reserved",
									corev1.LabelTopologyZone: "test-zone-4",
								}),
								Price: 4.020,
							},
						},
					}),
				}
			})
			It("should fail with requirements overlays overlap on zone", func() {
				overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-a",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      corev1.LabelTopologyZone,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"test-zone-1", "test-zone-4"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("1.03"),
					},
				})
				overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-b",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.CapacityTypeLabelKey,
								Operator: corev1.NodeSelectorOpExists,
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("23"),
					},
				})
				ExpectApplied(ctx, env.Client, overlayA, overlayB)

				// Reconcile both overlays
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				// Check that the conditions were set correctly
				updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
				Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
				Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

				updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
				Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
			})
			It("should fail with requirements overlays overlap on capacity type", func() {
				overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-a",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.CapacityTypeLabelKey,
								Operator: corev1.NodeSelectorOpExists,
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("1.03"),
					},
				})
				overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-b",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.CapacityTypeLabelKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"spot"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("23"),
					},
				})
				ExpectApplied(ctx, env.Client, overlayA, overlayB)

				// Reconcile both overlays
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				// Check that the conditions were set correctly
				updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
				Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
				Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

				updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
				Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
			})
			It("should succeed with requirements overlays don't overlap", func() {
				overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-a",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      corev1.LabelTopologyZone,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"test-zone-6", "test-zone-4"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("1.03"),
					},
				})
				overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-b",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.CapacityTypeLabelKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"spot"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("23"),
					},
				})
				ExpectApplied(ctx, env.Client, overlayA, overlayB)

				// Reconcile both overlays
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				// Check that the conditions were set correctly
				updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
				Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

				updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
				Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
			})
			It("should fail with requirements overlays overlap", func() {
				overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-a",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      corev1.LabelTopologyZone,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"test-zone-1", "test-zone-4"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("1.03"),
					},
				})
				overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{
						Name: "overlay-b",
					},
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.CapacityTypeLabelKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"spot"},
							},
						},
						Weight: lo.ToPtr(int32(10)),
						Price:  lo.ToPtr("23"),
					},
				})
				ExpectApplied(ctx, env.Client, overlayA, overlayB)

				// Reconcile both overlays
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				// Check that the conditions were set correctly
				updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
				Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
				Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

				updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
				Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
			})
		})
	})
	Context("Price", func() {
		It("should fail with conflicting price overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("1.03"),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("23"),
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should fail with price are the same overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("100"),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("100"),
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting price overlays with mutually exclusive requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("54"),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpNotIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("23"),
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting price overlays with mutually exclusive weights", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("34"),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(20)),
					Price:  lo.ToPtr("2"),
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
	})
	Context("Pricing adjustment", func() {
		It("should fail with conflicting price adjustment overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("+54"),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("-2"),
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should fail with pricing adjustment are the same overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("+100"),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("+100"),
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting price adjustments overlays with mutually exclusive requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("+54%"),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpNotIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("-2%"),
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting price adjustment overlays with mutually exclusive weights", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("-4%"),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(20)),
					PriceAdjustment: lo.ToPtr("-2%"),
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

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
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("54"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should fail with conflicting capacity overlays with multiple overlays", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "conflicting-overlay-1",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("hugepage-1Gi"): resource.MustParse("2Gi"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "good-overlay-1",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
				},
			})
			overlayC := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "conflicting-overlay-2",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("hugepage-1Gi"): resource.MustParse("3Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB, overlayC)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayC := ExpectExists(ctx, env.Client, overlayC)
			Expect(updatedOverlayC.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with capacity adjustment are the same overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("54"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("54"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting capacity overlays with mutually exclusive requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("54"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpNotIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("5"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with conflicting capacity overlays with mutually exclusive weights", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("55"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(20)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("5"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
		It("should pass with non conflicting capacity overlays with overlapping requirements", func() {
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("55"),
					},
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-b",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(20)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/buz"): resource.MustParse("5"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, overlayA, overlayB)

			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		})
	})
	Context("AdjustedPrice", func() {
		DescribeTable("should adjust price based overlay values",
			func(priceAdjustment string, basePrice float64, expectedPrice float64) {
				adjustedPrice := nodeoverlay.AdjustedPrice(basePrice, lo.ToPtr(priceAdjustment))
				Expect(adjustedPrice).To(BeNumerically("==", expectedPrice))
			},
			// Percentage adjustment
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
			adjustedPrice := nodeoverlay.AdjustedPrice(82781.0, lo.ToPtr("80.0"))
			Expect(adjustedPrice).To(BeNumerically("==", 80))
		})
		It("should provide the same if price or priceAdjustment is not provided", func() {
			adjustedPrice := nodeoverlay.AdjustedPrice(82781.0, lo.ToPtr(""))
			Expect(adjustedPrice).To(BeNumerically("==", 82781))
		})
	})
})
