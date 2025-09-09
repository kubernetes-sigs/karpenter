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
	nodePoolTwo           *v1.NodePool
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
	nodePoolTwo = test.NodePool()
	cloudProvider.Reset()
	store.Reset()

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
			},
		}),
	}
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
							// Key will fail runtime validation when applying the node overlay.
							// This will be due to the length of the key
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
				cloudProvider.Reset()
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
})

var _ = Describe("Instance Type Controller", func() {
	Context("Price Adjustment", func() {
		Context("Requirements", func() {
			It("should only apply price adjustments for nodepool that defined in the overlay requirements", func() {
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.NodePoolLabelKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodePool.Name},
							},
						},
						PriceAdjustment: lo.ToPtr("+1000.0"),
						Weight:          lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1001.020))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1.020))
			})
			It("should only apply price adjustments for nodeclass that defined in the overlay requirements", func() {
				nodePoolTwo.Spec.Template.Spec.NodeClassRef.Name = "unique-nodeclass"
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.NodeClassLabelKey(nodePool.Spec.Template.Spec.NodeClassRef.GroupKind()),
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodePool.Spec.Template.Spec.NodeClassRef.Name},
							},
						},
						PriceAdjustment: lo.ToPtr("+1000.0"),
						Weight:          lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1001.020))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1.020))
			})
			It("should only apply price adjustments for nodeclaim spec labels that defined in the overlay requirements", func() {
				nodePoolTwo.Spec.Template.Labels = lo.Assign(nodePoolTwo.Spec.Template.Labels, map[string]string{
					"test-label": "test-value",
				})
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      "test-label",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"test-value"},
							},
						},
						PriceAdjustment: lo.ToPtr("+1000.0"),
						Weight:          lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1.020))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1001.020))
			})
		})
		It("should not apply adjustments for invalid overlays", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							// Key will fail runtime validation when applying the node overlay.
							// This will be due to the length of the key
							Key:      fmt.Sprintf("test.com.test/test-%s", strings.ToLower(randomdata.Alphanumeric(250))),
							Operator: corev1.NodeSelectorOpExists,
						},
					},
					PriceAdjustment: lo.ToPtr("+1000.0"),
					Weight:          lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlay)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1.020))
		})
		It("should not apply adjustments for overlays that do not overlap", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type-not-found"},
						},
					},
					PriceAdjustment: lo.ToPtr("+1000.0"),
					Weight:          lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1.020))
		})
		It("should apply pricing adjustments for instances types", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					PriceAdjustment: lo.ToPtr("+1000.0"),
					Weight:          lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1001.020))
		})
		It("should apply pricing adjustments for instances types for capacity type", func() {
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
								corev1.LabelTopologyZone: "test-zone-1",
							}),
							Price: 5.020,
						},
					},
				}),
			}
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      v1.CapacityTypeLabelKey,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"on-demand"},
						},
					},
					PriceAdjustment: lo.ToPtr("+1000.0"),
					Weight:          lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 2))
			odReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      v1.CapacityTypeLabelKey,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"on-demand"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(odReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(odReq)[0].Price).To(BeNumerically("~", 1005.020))
			spotReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      v1.CapacityTypeLabelKey,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"spot"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(spotReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(spotReq)[0].Price).To(BeNumerically("~", 1.020))
		})
		It("should apply pricing adjustments for instances types for availability zone", func() {
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
								v1.CapacityTypeLabelKey:  "spot",
								corev1.LabelTopologyZone: "test-zone-2",
							}),
							Price: 5.020,
						},
					},
				}),
			}
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					PriceAdjustment: lo.ToPtr("+1000.0"),
					Weight:          lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 2))
			zoneOneReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-1"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneOneReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneOneReq)[0].Price).To(BeNumerically("~", 1001.020))
			zoneTwoReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneTwoReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneTwoReq)[0].Price).To(BeNumerically("~", 5.020))
		})
		It("should update price adjustment offerings instance types from multiple overlays", func() {
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
								v1.CapacityTypeLabelKey:  "reserved",
								corev1.LabelTopologyZone: "test-zone-4",
							}),
							Price: 4.020,
						},
					},
				}),
			}
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2", "test-zone-4"},
						},
					},
					PriceAdjustment: lo.ToPtr("+201"),
					Weight:          lo.ToPtr(int32(10)),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      v1.CapacityTypeLabelKey,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"spot"},
						},
					},
					PriceAdjustment: lo.ToPtr("-0.5"),
					Weight:          lo.ToPtr(int32(10)),
				},
			})
			// should not be valid
			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 3))
			zoneOneReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneOneReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneOneReq)[0].Price).To(BeNumerically("~", 203.020))
			zoneTwoReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-4"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneTwoReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneTwoReq)[0].Price).To(BeNumerically("~", 205.020))
			capacityReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      v1.CapacityTypeLabelKey,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"spot"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(capacityReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(capacityReq)[0].Price).To(BeNumerically("~", 0.52))
		})
		It("should update price adjustment offerings instance types from multiple overlays with different weights", func() {
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
								v1.CapacityTypeLabelKey:  "reserved",
								corev1.LabelTopologyZone: "test-zone-4",
							}),
							Price: 4.020,
						},
					},
				}),
			}
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a-test-100",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2"},
						},
					},
					PriceAdjustment: lo.ToPtr("+201"),
					Weight:          lo.ToPtr(int32(20)),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "b-test-100",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2", "test-zone-4"},
						},
					},
					PriceAdjustment: lo.ToPtr("-1.50"),
					Weight:          lo.ToPtr(int32(10)),
				},
			})
			// should not be valid
			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 3))
			zoneOneReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneOneReq))).To(BeNumerically("~", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneOneReq)[0].Price).To(BeNumerically("~", 203.020))
			zoneTwoReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-4"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneTwoReq))).To(BeNumerically("~", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneTwoReq)[0].Price).To(BeNumerically("~", 2.520))
		})
		It("should that there is not a partial application for instance types", func() {
			cloudProvider.InstanceTypes = nil
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type", "gpu-vendor-b-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("-23"),
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
							Values:   []string{"small-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("+10"),
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "small-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				}
			}
		})
		It("should that there is not a partial application for nodepools", func() {
			cloudProvider.InstanceTypes = nil
			nodePool = test.ReplaceRequirements(nodePool, v1.NodeSelectorRequirementWithMinValues{
				NodeSelectorRequirement: corev1.NodeSelectorRequirement{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"default-instance-type", "gpu-vendor-instance-type", "gpu-vendor-b-instance-type"},
				}})
			nodePoolTwo = test.ReplaceRequirements(test.NodePool(), v1.NodeSelectorRequirementWithMinValues{
				NodeSelectorRequirement: corev1.NodeSelectorRequirement{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"gpu-vendor-b-instance-type"},
				}})
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type", "gpu-vendor-b-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("+10"),
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
							Values:   []string{"small-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight:          lo.ToPtr(int32(10)),
					PriceAdjustment: lo.ToPtr("-23"),
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlayA, overlayB)
			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				case "small-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				}
			}

			instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				case "small-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				}
			}
		})
	})
	Context("Price", func() {
		Context("Requirements", func() {
			It("should only apply override for nodepool that defined in the overlay requirements", func() {
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.NodePoolLabelKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodePool.Name},
							},
						},
						Price:  lo.ToPtr("13234.223"),
						Weight: lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 13234.223))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1.020))
			})
			It("should only apply price adjustments for nodeclass that defined in the overlay requirements", func() {
				nodePoolTwo.Spec.Template.Spec.NodeClassRef.Name = "unique-nodeclass"
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.NodeClassLabelKey(nodePool.Spec.Template.Spec.NodeClassRef.GroupKind()),
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodePool.Spec.Template.Spec.NodeClassRef.Name},
							},
						},
						Price:  lo.ToPtr("13234.223"),
						Weight: lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 13234.223))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1.020))
			})
			It("should only apply price adjustments for nodeclaim spec labels that defined in the overlay requirements", func() {
				nodePoolTwo.Spec.Template.Labels = lo.Assign(nodePoolTwo.Spec.Template.Labels, map[string]string{
					"test-label": "test-value",
				})
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      "test-label",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"test-value"},
							},
						},
						Price:  lo.ToPtr("13234.223"),
						Weight: lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 1.020))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("==", 13234.223))
			})
		})
		It("should not override instance type price for invalid overlays", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							// Key will fail runtime validation when applying the node overlay.
							// This will be due to the length of the key
							Key:      fmt.Sprintf("test.com.test/test-%s", strings.ToLower(randomdata.Alphanumeric(250))),
							Operator: corev1.NodeSelectorOpDoesNotExist,
						},
					},
					Price:  lo.ToPtr("13234.223"),
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlay)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("~", 1.020))
		})
		It("should not override instance type price for overlays that do not overlap", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type-not-found"},
						},
					},
					Price:  lo.ToPtr("13234.223"),
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("~", 1.020))
		})
		It("should override instance type price", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type"},
						},
					},
					Price:  lo.ToPtr("13234.223"),
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("~", 13234.223))
		})
		It("should override instance type price by capacity type", func() {
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
								corev1.LabelTopologyZone: "test-zone-1",
							}),
							Price: 5.020,
						},
					},
				}),
			}
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      v1.CapacityTypeLabelKey,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"on-demand"},
						},
					},
					Price:  lo.ToPtr("12321.32"),
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 2))
			odReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      v1.CapacityTypeLabelKey,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"on-demand"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(odReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(odReq)[0].Price).To(BeNumerically("~", 12321.32))
			spotReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      v1.CapacityTypeLabelKey,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"spot"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(spotReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(spotReq)[0].Price).To(BeNumerically("~", 1.020))
		})
		It("should override instance type price by availability zone", func() {
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
								v1.CapacityTypeLabelKey:  "spot",
								corev1.LabelTopologyZone: "test-zone-2",
							}),
							Price: 5.020,
						},
					},
				}),
			}
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					Price:  lo.ToPtr("121.421"),
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 2))
			zoneOneReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-1"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneOneReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneOneReq)[0].Price).To(BeNumerically("~", 121.421))
			zoneTwoReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneTwoReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneTwoReq)[0].Price).To(BeNumerically("~", 5.020))
		})
		It("should update price offerings instance types from multiple overlays", func() {
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
								v1.CapacityTypeLabelKey:  "reserved",
								corev1.LabelTopologyZone: "test-zone-4",
							}),
							Price: 4.020,
						},
					},
				}),
			}
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2", "test-zone-4"},
						},
					},
					Price:  lo.ToPtr("121.421"),
					Weight: lo.ToPtr(int32(10)),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      v1.CapacityTypeLabelKey,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"spot"},
						},
					},
					Price:  lo.ToPtr("165.421"),
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 3))
			zoneOneReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneOneReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneOneReq)[0].Price).To(BeNumerically("~", 121.421))
			zoneTwoReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-4"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneTwoReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneTwoReq)[0].Price).To(BeNumerically("~", 121.421))
			capacityReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      v1.CapacityTypeLabelKey,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"spot"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(capacityReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(capacityReq)[0].Price).To(BeNumerically("~", 165.421))
		})
		It("should update price offerings instance types from multiple overlays with different weights", func() {
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
								v1.CapacityTypeLabelKey:  "reserved",
								corev1.LabelTopologyZone: "test-zone-4",
							}),
							Price: 4.020,
						},
					},
				}),
			}
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2"},
						},
					},
					Price:  lo.ToPtr("121.421"),
					Weight: lo.ToPtr(int32(20)),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2", "test-zone-4"},
						},
					},
					Price:  lo.ToPtr("165.421"),
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 3))
			zoneOneReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneOneReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneOneReq)[0].Price).To(BeNumerically("~", 121.421))
			zoneTwoReq := scheduling.NewNodeSelectorRequirements(corev1.NodeSelectorRequirement{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-4"},
			})
			Expect(len(instanceTypeList[0].Offerings.Compatible(zoneTwoReq))).To(BeNumerically("==", 1))
			Expect(instanceTypeList[0].Offerings.Compatible(zoneTwoReq)[0].Price).To(BeNumerically("~", 165.421))
		})
		It("should that there is not a partial application for instance types", func() {
			cloudProvider.InstanceTypes = nil
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type", "gpu-vendor-b-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("382"),
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
							Values:   []string{"small-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("100"),
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "small-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				}
			}
		})
		It("should that there is not a partial application for nodepools", func() {
			cloudProvider.InstanceTypes = nil
			nodePool = test.ReplaceRequirements(nodePool, v1.NodeSelectorRequirementWithMinValues{
				NodeSelectorRequirement: corev1.NodeSelectorRequirement{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"default-instance-type", "gpu-vendor-instance-type", "gpu-vendor-b-instance-type"},
				}})
			nodePoolTwo = test.ReplaceRequirements(test.NodePool(), v1.NodeSelectorRequirementWithMinValues{
				NodeSelectorRequirement: corev1.NodeSelectorRequirement{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"gpu-vendor-b-instance-type"},
				}})
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
					Price:  lo.ToPtr("382"),
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
							Values:   []string{"small-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Price:  lo.ToPtr("100"),
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlayA, overlayB)
			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				case "small-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				}
			}

			instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				case "small-instance-type":
					Expect(it.IsPricingOverlayApplied()).To(BeTrue())
				}
			}
		})
	})
	Context("Capacity", func() {
		Context("Requirements", func() {
			It("should only apply override for nodepool that defined in the overlay requirements", func() {
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.NodePoolLabelKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodePool.Name},
							},
						},
						Capacity: corev1.ResourceList{
							corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
						},
						Weight: lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).To(BeNil())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				resource, exist := instanceTypeList[0].Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
				Expect(exist).To(BeTrue())
				Expect(resource).To(BeNumerically("==", 1))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(lo.Keys(instanceTypeList[0].Capacity)).ToNot(ContainElement("smarter-devices/fuse"))
			})
			It("should only apply price adjustments for nodeclass that defined in the overlay requirements", func() {
				nodePoolTwo.Spec.Template.Spec.NodeClassRef.Name = "unique-nodeclass"
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      v1.NodeClassLabelKey(nodePool.Spec.Template.Spec.NodeClassRef.GroupKind()),
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodePool.Spec.Template.Spec.NodeClassRef.Name},
							},
						},
						Capacity: corev1.ResourceList{
							corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
						},
						Weight: lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).ToNot(HaveOccurred())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				resource, exist := instanceTypeList[0].Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
				Expect(exist).To(BeTrue())
				Expect(resource).To(BeNumerically("==", 1))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
				Expect(err).ToNot(HaveOccurred())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(lo.Keys(instanceTypeList[0].Capacity)).ToNot(ContainElement("smarter-devices/fuse"))
			})
			It("should only apply price adjustments for nodeclaim spec labels that defined in the overlay requirements", func() {
				nodePoolTwo.Spec.Template.Labels = lo.Assign(nodePoolTwo.Spec.Template.Labels, map[string]string{
					"test-label": "test-value",
				})
				overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
					Spec: v1alpha1.NodeOverlaySpec{
						Requirements: []corev1.NodeSelectorRequirement{
							{
								Key:      "test-label",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"test-value"},
							},
						},
						Capacity: corev1.ResourceList{
							corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
						},
						Weight: lo.ToPtr(int32(10)),
					},
				})
				ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlay)
				ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

				instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
				Expect(err).ToNot(HaveOccurred())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				Expect(lo.Keys(instanceTypeList[0].Capacity)).ToNot(ContainElement("smarter-devices/fuse"))

				instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
				Expect(err).To(BeNil())
				instanceTypeList, err = store.ApplyAll(nodePoolTwo.Name, instanceTypeList)
				Expect(err).ToNot(HaveOccurred())

				Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
				Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
				resource, exist := instanceTypeList[0].Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
				Expect(exist).To(BeTrue())
				Expect(resource).To(BeNumerically("==", 1))
			})
		})
		It("should not apply capacity adjustments for invalid overlay", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							// Key will fail runtime validation when applying the node overlay.
							// This will be due to the length of the key
							Key:      fmt.Sprintf("test.com.test/test-%s", strings.ToLower(randomdata.Alphanumeric(250))),
							Operator: corev1.NodeSelectorOpExists,
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).ToNot(HaveOccurred())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			Expect(lo.Keys(instanceTypeList[0].Capacity)).ToNot(ContainElement("smarter-devices/fuse"))
		})
		It("should not apply capacity adjustments for instances types that do not overlap", func() {
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type-not-found"},
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).ToNot(HaveOccurred())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			Expect(lo.Keys(instanceTypeList[0].Capacity)).ToNot(ContainElement("smarter-devices/fuse"))
		})
		It("should apply capacity adjustments for instances types", func() {
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
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).ToNot(HaveOccurred())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
			Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
			resource, exist := instanceTypeList[0].Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
			Expect(exist).To(BeTrue())
			Expect(resource).To(BeNumerically("==", 1))
		})
		It("should update capacity for instance types from multiple overlays", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "default-instance-type-zone-one",
					Offerings: []*cloudprovider.Offering{
						{
							Available: true,
							Requirements: scheduling.NewLabelRequirements(map[string]string{
								v1.CapacityTypeLabelKey:  "spot",
								corev1.LabelTopologyZone: "test-zone-1",
							}),
							Price: 1.020,
						},
					},
				}),
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "default-instance-type-zone-two",
					Offerings: []*cloudprovider.Offering{
						{
							Available: true,
							Requirements: scheduling.NewLabelRequirements(map[string]string{
								v1.CapacityTypeLabelKey:  "on-demand",
								corev1.LabelTopologyZone: "test-zone-2",
							}),
							Price: 2.020,
						},
					},
				}),
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "default-instance-type-zone-four",
					Offerings: []*cloudprovider.Offering{
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
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2", "test-zone-4"},
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      v1.CapacityTypeLabelKey,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"spot"},
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())
			Expect(len(instanceTypeList)).To(BeNumerically("==", 3))

			for _, it := range instanceTypeList {
				if it.Name == "default-instance-type-zone-one" {
					Expect(len(it.Offerings)).To(BeNumerically("==", 1))
					Expect(it.Offerings[0].Price).To(BeNumerically("~", 1.020))
					resource := it.Capacity.Name(corev1.ResourceName("hugepages-1Gi"), resource.DecimalSI).String()
					Expect(resource).To(Equal("2Gi"))
				}
				if it.Name == "default-instance-type-zone-two" {
					Expect(len(it.Offerings)).To(BeNumerically("==", 1))
					Expect(it.Offerings[0].Price).To(BeNumerically("~", 2.020))
					resource, exist := it.Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
					Expect(exist).To(BeTrue())
					Expect(resource).To(BeNumerically("==", 1))
				}
				if it.Name == "default-instance-type-zone-four" {
					Expect(len(it.Offerings)).To(BeNumerically("==", 1))
					Expect(it.Offerings[0].Price).To(BeNumerically("~", 4.020))
					resource, exist := it.Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
					Expect(exist).To(BeTrue())
					Expect(resource).To(BeNumerically("==", 1))
				}
			}
		})
		It("should update capacity for one instance types from multiple overlays", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "default-instance-type-zone-one",
					Offerings: []*cloudprovider.Offering{
						{
							Available: true,
							Requirements: scheduling.NewLabelRequirements(map[string]string{
								v1.CapacityTypeLabelKey:  "spot",
								corev1.LabelTopologyZone: "test-zone-1",
							}),
							Price: 1.020,
						},
					},
				}),
			}
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type-zone-one"},
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type-zone-one"},
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())
			Expect(len(instanceTypeList)).To(BeNumerically("==", 1))

			for _, it := range instanceTypeList {
				if it.Name == "default-instance-type-zone-one" {
					Expect(len(it.Offerings)).To(BeNumerically("==", 1))
					Expect(it.Offerings[0].Price).To(BeNumerically("~", 1.020))
					resourceString := it.Capacity.Name(corev1.ResourceName("hugepages-1Gi"), resource.DecimalSI).String()
					Expect(resourceString).To(Equal("2Gi"))
					resourceNumber, exist := it.Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
					Expect(exist).To(BeTrue())
					Expect(resourceNumber).To(BeNumerically("==", 1))
				}
			}
		})
		It("should update price offerings instance types from multiple overlays with different weights", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "default-instance-type-zone-one",
					Offerings: []*cloudprovider.Offering{
						{
							Available: true,
							Requirements: scheduling.NewLabelRequirements(map[string]string{
								v1.CapacityTypeLabelKey:  "spot",
								corev1.LabelTopologyZone: "test-zone-1",
							}),
							Price: 1.020,
						},
					},
				}),
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "default-instance-type-zone-two",
					Offerings: []*cloudprovider.Offering{
						{
							Available: true,
							Requirements: scheduling.NewLabelRequirements(map[string]string{
								v1.CapacityTypeLabelKey:  "on-demand",
								corev1.LabelTopologyZone: "test-zone-2",
							}),
							Price: 2.020,
						},
					},
				}),
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "default-instance-type-zone-four",
					Offerings: []*cloudprovider.Offering{
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
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2"},
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
					},
					Weight: lo.ToPtr(int32(20)),
				},
			})
			overlayB := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"test-zone-2", "test-zone-4"},
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
					},
					Weight: lo.ToPtr(int32(10)),
				},
			})
			// should not be valid
			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())
			Expect(len(instanceTypeList)).To(BeNumerically("==", 3))

			for _, it := range instanceTypeList {
				if it.Name == "default-instance-type-zone-two" {
					Expect(len(it.Offerings)).To(BeNumerically("==", 1))
					Expect(it.Offerings[0].Price).To(BeNumerically("~", 2.020))
					resource, exist := it.Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
					Expect(exist).To(BeTrue())
					Expect(resource).To(BeNumerically("==", 1))
				}
				if it.Name == "default-instance-type-zone-four" {
					Expect(len(it.Offerings)).To(BeNumerically("==", 1))
					Expect(it.Offerings[0].Price).To(BeNumerically("~", 4.020))
					resource := it.Capacity.Name(corev1.ResourceName("hugepages-1Gi"), resource.DecimalSI).String()
					Expect(resource).To(Equal("2Gi"))
				}
			}
		})
		It("should that there is not a partial application for instance types", func() {
			cloudProvider.InstanceTypes = nil
			overlayA := test.NodeOverlay(v1alpha1.NodeOverlay{
				ObjectMeta: metav1.ObjectMeta{
					Name: "overlay-a",
				},
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelInstanceTypeStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"default-instance-type", "small-instance-type", "gpu-vendor-b-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/buz"): resource.MustParse("54"),
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
							Values:   []string{"small-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/buz"): resource.MustParse("21"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeTrue())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeFalse())
				case "small-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeTrue())
				}
			}
		})
		It("should that there is not a partial application for nodepools", func() {
			cloudProvider.InstanceTypes = nil
			nodePool = test.ReplaceRequirements(nodePool, v1.NodeSelectorRequirementWithMinValues{
				NodeSelectorRequirement: corev1.NodeSelectorRequirement{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"default-instance-type", "gpu-vendor-instance-type", "gpu-vendor-b-instance-type"},
				}})
			nodePoolTwo := test.ReplaceRequirements(test.NodePool(), v1.NodeSelectorRequirementWithMinValues{
				NodeSelectorRequirement: corev1.NodeSelectorRequirement{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"gpu-vendor-b-instance-type"},
				}})
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
						corev1.ResourceName("smarter-devices/buz"): resource.MustParse("54"),
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
							Values:   []string{"small-instance-type", "gpu-vendor-instance-type"},
						},
					},
					Weight: lo.ToPtr(int32(10)),
					Capacity: corev1.ResourceList{
						corev1.ResourceName("smarter-devices/buz"): resource.MustParse("21"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, nodePoolTwo, overlayA, overlayB)
			// Reconcile both overlays
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// Check that the conditions were set correctly
			updatedOverlayA := ExpectExists(ctx, env.Client, overlayA)
			Expect(updatedOverlayA.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updatedOverlayA.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))

			updatedOverlayB := ExpectExists(ctx, env.Client, overlayB)
			Expect(updatedOverlayB.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())

			instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeFalse())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeTrue())
				case "small-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeTrue())
				}
			}

			instanceTypeList, err = cloudProvider.GetInstanceTypes(ctx, nodePoolTwo)
			Expect(err).To(BeNil())
			instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
			Expect(err).To(BeNil())

			Expect(len(instanceTypeList)).To(BeNumerically("==", 6))
			for _, it := range instanceTypeList {
				switch it.Name {
				case "default-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeFalse())
				case "gpu-vendor-b-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeFalse())
				case "gpu-vendor-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeTrue())
				case "small-instance-type":
					Expect(it.IsCapacityOverlayApplied()).To(BeTrue())
				}
			}
		})
	})
	It("should apply pricing and capacity adjustment from two overlays on the same instance type", func() {
		overlayPrice := test.NodeOverlay(v1alpha1.NodeOverlay{
			Spec: v1alpha1.NodeOverlaySpec{
				Requirements: []corev1.NodeSelectorRequirement{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"default-instance-type"},
					},
				},
				PriceAdjustment: lo.ToPtr("+1000.0"),
				Weight:          lo.ToPtr(int32(10)),
			},
		})
		overlayCapacity := test.NodeOverlay(v1alpha1.NodeOverlay{
			Spec: v1alpha1.NodeOverlaySpec{
				Requirements: []corev1.NodeSelectorRequirement{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"default-instance-type"},
					},
				},
				Capacity: corev1.ResourceList{
					corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
				},
				Weight: lo.ToPtr(int32(10)),
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, overlayPrice, overlayCapacity)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
		Expect(err).To(BeNil())
		instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
		Expect(err).To(BeNil())

		Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
		Expect(len(instanceTypeList[0].Offerings)).To(BeNumerically("==", 1))
		Expect(instanceTypeList[0].Offerings[0].Price).To(BeNumerically("~", 1001.020))
		resource, exist := instanceTypeList[0].Capacity.Name(corev1.ResourceName("smarter-devices/fuse"), resource.DecimalSI).AsInt64()
		Expect(exist).To(BeTrue())
		Expect(resource).To(BeNumerically("==", 1))
	})
	It("should have an empty instance types set when cloudprovider does not return instance types", func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{}
		overlayPrice := test.NodeOverlay(v1alpha1.NodeOverlay{
			Spec: v1alpha1.NodeOverlaySpec{
				Requirements: []corev1.NodeSelectorRequirement{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"default-instance-type"},
					},
				},
				Capacity: corev1.ResourceList{
					corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
				},
				Weight: lo.ToPtr(int32(10)),
			},
		})

		ExpectApplied(ctx, env.Client, nodePool, overlayPrice)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
		Expect(err).To(BeNil())
		instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
		Expect(err).To(BeNil())
		Expect(len(instanceTypeList)).To(BeNumerically("==", 0))
	})
	It("should have an empty instance types set when cloudprovider return an error", func() {
		cloudProvider.ErrorsForNodePool = map[string]error{nodePool.Name: fmt.Errorf("test error")}
		overlayPrice := test.NodeOverlay(v1alpha1.NodeOverlay{
			Spec: v1alpha1.NodeOverlaySpec{
				Requirements: []corev1.NodeSelectorRequirement{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"default-instance-type"},
					},
				},
				Capacity: corev1.ResourceList{
					corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
				},
				Weight: lo.ToPtr(int32(10)),
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, overlayPrice)
		ExpectReconciledFailed(ctx, nodeOverlayController, reconcile.Request{})

		instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
		Expect(err).ToNot(BeNil())
		instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
		Expect(err).ToNot(BeNil())
		Expect(len(instanceTypeList)).To(BeNumerically("==", 0))
	})
	It("should not return instance type requirements with nodepool, nodeclass, and custom nodepool labels", func() {
		nodePool.Spec.Template.Labels = lo.Assign(nodePool.Spec.Template.Labels, map[string]string{
			"test-label": "test-value",
		})
		overlayPrice := test.NodeOverlay(v1alpha1.NodeOverlay{
			Spec: v1alpha1.NodeOverlaySpec{
				Requirements: []corev1.NodeSelectorRequirement{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"default-instance-type"},
					},
				},
				Capacity: corev1.ResourceList{
					corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
				},
				Weight: lo.ToPtr(int32(10)),
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, overlayPrice)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		instanceTypeList, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
		Expect(err).To(BeNil())
		instanceTypeList, err = store.ApplyAll(nodePool.Name, instanceTypeList)
		Expect(err).To(BeNil())
		Expect(len(instanceTypeList)).To(BeNumerically("==", 1))
		Expect(instanceTypeList[0].Requirements.Keys()).NotTo(ContainElement(v1.NodePoolLabelKey))
		Expect(instanceTypeList[0].Requirements.Keys()).NotTo(ContainElement(v1.NodeClassLabelKey(nodePool.Spec.Template.Spec.NodeClassRef.GroupKind())))
		Expect(instanceTypeList[0].Requirements.Keys()).NotTo(ContainElements(lo.Keys(nodePool.Spec.Template.Labels)))
	})
})
