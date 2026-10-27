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

package nodeoverlay

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const hugePages2Mi corev1.ResourceName = "hugepages-2Mi"

func maxCapacityOverlay(name string, weight int32, capacity, maxCapacity corev1.ResourceList) *v1alpha1.NodeOverlay {
	return test.NodeOverlay(v1alpha1.NodeOverlay{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.NodeOverlaySpec{
			Requirements: []v1alpha1.NodeSelectorRequirement{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"default-instance-type"}},
			},
			Weight:      new(weight),
			Capacity:    capacity,
			MaxCapacity: maxCapacity,
		},
	})
}

func expectOverlaidInstanceType() *cloudprovider.InstanceType {
	GinkgoHelper()
	instanceTypes, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
	Expect(err).ToNot(HaveOccurred())
	instanceTypes, err = store.ApplyAll(nodePool.Name, instanceTypes)
	Expect(err).ToNot(HaveOccurred())
	Expect(instanceTypes).To(HaveLen(1))
	return instanceTypes[0]
}

var _ = Describe("Max Capacity", func() {
	It("should make a resource flexible and set its ceiling", func() {
		overlay := maxCapacityOverlay("overlay", 10, nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("4Gi")})
		ExpectApplied(ctx, env.Client, nodePool, overlay)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		Expect(ExpectExists(ctx, env.Client, overlay).StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		it := expectOverlaidInstanceType()
		Expect(it.IsFlexible(hugePages2Mi)).To(BeTrue())
		Expect(it.IsCapacityOverlayApplied()).To(BeTrue())
		Expect(it.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("4Gi")))
	})
	It("should let a higher weight capacity win over a lower weight maxCapacity for the same resource", func() {
		fixed := maxCapacityOverlay("fixed", 20, corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")}, nil)
		flexible := maxCapacityOverlay("flexible", 10, nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("4Gi")})
		ExpectApplied(ctx, env.Client, nodePool, fixed, flexible)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		Expect(ExpectExists(ctx, env.Client, fixed).StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		Expect(ExpectExists(ctx, env.Client, flexible).StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeTrue())
		it := expectOverlaidInstanceType()
		Expect(it.IsFlexible(hugePages2Mi)).To(BeFalse())
		Expect(it.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("2Gi")))
	})
	It("should let a higher weight maxCapacity win over a lower weight capacity for the same resource", func() {
		fixed := maxCapacityOverlay("fixed", 10, corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")}, nil)
		flexible := maxCapacityOverlay("flexible", 20, nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("4Gi")})
		ExpectApplied(ctx, env.Client, nodePool, fixed, flexible)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		it := expectOverlaidInstanceType()
		Expect(it.IsFlexible(hugePages2Mi)).To(BeTrue())
		Expect(it.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("4Gi")))
	})
	It("should merge capacity and maxCapacity for different resources", func() {
		fixed := maxCapacityOverlay("fixed", 20, corev1.ResourceList{"hugepages-1Gi": resource.MustParse("2Gi")}, nil)
		flexible := maxCapacityOverlay("flexible", 10, nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("4Gi")})
		ExpectApplied(ctx, env.Client, nodePool, fixed, flexible)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		it := expectOverlaidInstanceType()
		Expect(it.IsFlexible("hugepages-1Gi")).To(BeFalse())
		Expect(it.IsFlexible(hugePages2Mi)).To(BeTrue())
		Expect(it.Capacity).To(HaveKeyWithValue(corev1.ResourceName("hugepages-1Gi"), resource.MustParse("2Gi")))
		Expect(it.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("4Gi")))
	})
	DescribeTable("should mark a conflict when overlays of the same weight set the same resource",
		func(capacityFirst bool) {
			fixed := maxCapacityOverlay("overlay-a", 10, corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")}, nil)
			flexible := maxCapacityOverlay("overlay-b", 10, nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("4Gi")})
			if !capacityFirst {
				fixed.Name, flexible.Name = flexible.Name, fixed.Name
			}
			ExpectApplied(ctx, env.Client, nodePool, fixed, flexible)
			ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

			// overlay-b is evaluated first at equal weight, so overlay-a is the one in conflict
			conflicting := lo.Ternary(capacityFirst, fixed, flexible)
			updated := ExpectExists(ctx, env.Client, conflicting)
			Expect(updated.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
			Expect(updated.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))
		},
		Entry("capacity in the overlay evaluated last", true),
		Entry("maxCapacity in the overlay evaluated last", false),
	)
	It("should fail runtime validation for hugepages that are not a whole number of pages", func() {
		overlay := maxCapacityOverlay("overlay", 10, nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("3Mi")})
		ExpectApplied(ctx, env.Client, nodePool, overlay)
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		updated := ExpectExists(ctx, env.Client, overlay)
		Expect(updated.StatusConditions().IsTrue(v1alpha1.ConditionTypeValidationSucceeded)).To(BeFalse())
		Expect(updated.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("RuntimeValidation"))
	})
})

var _ = Describe("Store Apply Max Capacity", func() {
	It("should apply maxCapacity without changing the original instance type or the store", func() {
		instanceType := fake.NewInstanceType("m5.large")
		flexibleUpdate := corev1.ResourceList{hugePages2Mi: resource.MustParse("4Gi")}
		internal := newInternalInstanceTypeStore()
		internal.evaluatedNodePools.Insert("default")
		internal.updates = map[string]map[string]*instanceTypeUpdate{
			"default": {
				instanceType.Name: &instanceTypeUpdate{Capacity: &capacityUpdate{FlexibleUpdate: flexibleUpdate}},
			},
		}

		result := internal.apply("default", instanceType)

		Expect(result.IsFlexible(hugePages2Mi)).To(BeTrue())
		Expect(result.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("4Gi")))
		Expect(instanceType.Capacity).ToNot(HaveKey(hugePages2Mi))
		Expect(instanceType.IsFlexible(hugePages2Mi)).To(BeFalse())
		Expect(flexibleUpdate).To(HaveLen(1))
	})
	It("should give the same result as before for overlays with only capacity", func() {
		instanceType := fake.NewInstanceType("m5.large")
		internal := newInternalInstanceTypeStore()
		internal.evaluatedNodePools.Insert("default")
		internal.updateInstanceTypeCapacity("default", instanceType.Name, *maxCapacityOverlay("fixed", 10, corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")}, nil))

		result := internal.apply("default", instanceType)

		Expect(result.IsFlexible(hugePages2Mi)).To(BeFalse())
		Expect(result.FlexibleCapacity()).To(BeEmpty())
		Expect(result.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("2Gi")))
	})
})
