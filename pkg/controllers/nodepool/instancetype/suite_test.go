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

package instancetype_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	clock "k8s.io/utils/clock/testing"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/instancetype"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx                    context.Context
	env                    *test.Environment
	fakeClock              *clock.FakeClock
	cloudProvider          *fake.CloudProvider
	cluster                *state.Cluster
	nodePool               *v1.NodePool
	instanceTypeController *instancetype.Controller
)

func TestValidation(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodePool Instance Type")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(testv1alpha1.CRDs...))

	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	instanceTypeController = instancetype.NewController(env.Client, cloudProvider, cluster)
})

var _ = BeforeEach(func() {
	nodePool = test.NodePool()
	cluster.Reset()
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
})
var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Instance Type Controller", func() {
	Context("Price Adjustment", func() {
		It("should not apply adjustments for invalid overlays", func() {
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
			overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "Conflict", "testing")

			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			overlayA.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
			overlayB.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			overlayA.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
			overlayB.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
	})
	Context("Price", func() {
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
			overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			// should not be valid
			overlayA.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
			overlayB.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			// should not be valid
			overlayA.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
			overlayB.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
	})
	Context("Capacity", func() {
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
			overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlay)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
			Expect(err).To(BeNil())
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
			// should not be valid
			overlayA.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
			overlayB.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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
			overlayA.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
			overlayB.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

			ExpectApplied(ctx, env.Client, nodePool, overlayA, overlayB)
			ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

			instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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

		overlayPrice.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
		overlayCapacity.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

		ExpectApplied(ctx, env.Client, nodePool, overlayPrice, overlayCapacity)
		ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

		instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
		Expect(err).To(BeNil())
		for _, it := range instanceTypeList {
			fmt.Println(it.Name)
			fmt.Println(it.Capacity)
			for i, of := range it.Offerings {
				fmt.Println(fmt.Sprintf("offering %d: ", i), of.Price)
				fmt.Println(fmt.Sprintf("offering %d: ", i), of.Requirements.String())
				fmt.Println(fmt.Sprintf("offering %d: ", i), of.Available)
			}
		}
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

		overlayPrice.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

		ExpectApplied(ctx, env.Client, nodePool, overlayPrice)
		ExpectObjectReconciled(ctx, env.Client, instanceTypeController, nodePool)

		instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
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

		overlayPrice.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)

		ExpectApplied(ctx, env.Client, nodePool, overlayPrice)
		err := ExpectObjectReconcileFailed(ctx, env.Client, instanceTypeController, nodePool)
		Expect(err).ToNot(BeNil())

		instanceTypeList, err := cluster.GetInstanceTypes(nodePool.Name)
		Expect(err).To(BeNil())
		Expect(len(instanceTypeList)).To(BeNumerically("==", 0))
	})
})
