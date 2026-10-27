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
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const (
	hugePages2Mi corev1.ResourceName = "hugepages-2Mi"
	hugePages1Gi corev1.ResourceName = "hugepages-1Gi"
)

func value(s string) int64 {
	q := resource.MustParse(s)
	return q.Value()
}

func memoryOf(resources corev1.ResourceList) int64 {
	return resources.Memory().Value()
}

func newFlexibleTestInstanceType() *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		},
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
}

var _ = Describe("Flexible Capacity", func() {
	Context("InstanceType", func() {
		It("should not take flexible hugepages out of allocatable memory", func() {
			it := newFlexibleTestInstanceType()
			it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")})

			Expect(it.IsFlexible(hugePages2Mi)).To(BeTrue())
			Expect(it.IsCapacityOverlayApplied()).To(BeTrue())
			Expect(it.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("8Gi")))
			allocatable := it.Allocatable()
			Expect(allocatable.Memory().Value()).To(Equal(value("15Gi")))
			Expect(allocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("8Gi")))
		})
		It("should take fixed hugepages out of memory but not flexible hugepages of another size", func() {
			it := newFlexibleTestInstanceType()
			it.ApplyCapacityOverlay(corev1.ResourceList{hugePages1Gi: resource.MustParse("2Gi")})
			it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")})

			Expect(it.IsFlexible(hugePages1Gi)).To(BeFalse())
			Expect(it.IsFlexible(hugePages2Mi)).To(BeTrue())
			Expect(memoryOf(it.Allocatable())).To(Equal(value("13Gi")))
		})
		It("should stop treating a resource as flexible when a fixed amount is applied for it", func() {
			shared := corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")}
			it := newFlexibleTestInstanceType()
			it.ApplyMaxCapacityOverlay(shared)
			it.ApplyCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")})

			Expect(it.IsFlexible(hugePages2Mi)).To(BeFalse())
			Expect(memoryOf(it.Allocatable())).To(Equal(value("13Gi")))
			// The shared map must not change
			Expect(shared).To(HaveKey(hugePages2Mi))
		})
		It("should not change the maps it was given", func() {
			original := newFlexibleTestInstanceType()
			capacity := original.Capacity
			maxCapacity := corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")}
			original.ApplyMaxCapacityOverlay(maxCapacity)
			original.ApplyMaxCapacityOverlay(corev1.ResourceList{"example.com/device": resource.MustParse("4")})

			Expect(capacity).ToNot(HaveKey(hugePages2Mi))
			Expect(maxCapacity).To(HaveLen(1))
			Expect(original.IsFlexible("example.com/device")).To(BeTrue())
		})
		It("should keep flexible capacity on a deep copy", func() {
			it := newFlexibleTestInstanceType()
			it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")})
			copied := it.DeepCopy()

			Expect(copied.IsFlexible(hugePages2Mi)).To(BeTrue())
			Expect(copied.IsCapacityOverlayApplied()).To(BeTrue())
			Expect(memoryOf(copied.Allocatable())).To(Equal(value("15Gi")))
		})
		It("should keep a flexible resource flexible inside an offering capacity override group", func() {
			it := newFlexibleTestInstanceType()
			it.Offerings = cloudprovider.Offerings{
				{Available: true},
				{Available: true, CapacityOverride: corev1.ResourceList{hugePages2Mi: resource.MustParse("4Gi")}},
			}
			it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")})

			groups := it.AllocatableOfferingsList()
			Expect(groups).To(HaveLen(2))
			Expect(groups[1].Allocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("4Gi")))
			Expect(groups[1].Allocatable.Memory().Value()).To(Equal(value("15Gi")))
		})
	})
	Context("FitsAllocatable", func() {
		It("should behave like resources.Fits when nothing is flexible", func() {
			it := newFlexibleTestInstanceType()
			it.ApplyCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("4Gi")})
			allocatable := it.Allocatable()

			// Fixed hugepages are already out of memory: 16Gi - 1Gi - 4Gi = 11Gi
			Expect(it.FitsAllocatable(corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("11Gi"),
				hugePages2Mi:          resource.MustParse("4Gi"),
			}, allocatable)).To(BeTrue())
			Expect(it.FitsAllocatable(corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("12Gi"),
			}, allocatable)).To(BeFalse())
		})
		It("should charge flexible hugepages against memory", func() {
			it := newFlexibleTestInstanceType()
			it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")})
			allocatable := it.Allocatable()

			Expect(it.FitsAllocatable(corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("7Gi"),
				hugePages2Mi:          resource.MustParse("8Gi"),
			}, allocatable)).To(BeTrue())
			Expect(it.FitsAllocatable(corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("8Gi"),
				hugePages2Mi:          resource.MustParse("8Gi"),
			}, allocatable)).To(BeFalse())
			Expect(it.FitsAllocatable(corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("15Gi"),
			}, allocatable)).To(BeTrue())
		})
		It("should not fit a request above the ceiling", func() {
			it := newFlexibleTestInstanceType()
			it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")})

			Expect(it.FitsAllocatable(corev1.ResourceList{
				hugePages2Mi: resource.MustParse("4Gi"),
			}, it.Allocatable())).To(BeFalse())
		})
		It("should not charge flexible resources that are not hugepages against memory", func() {
			it := newFlexibleTestInstanceType()
			it.ApplyMaxCapacityOverlay(corev1.ResourceList{"example.com/device": resource.MustParse("4")})

			Expect(it.FitsAllocatable(corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("15Gi"),
				"example.com/device":  resource.MustParse("4"),
			}, it.Allocatable())).To(BeTrue())
		})
	})
	Context("Annotation", func() {
		It("should return nothing when the annotation is absent", func() {
			flexible, err := cloudprovider.FlexibleCapacityFromNodeClaim(&v1.NodeClaim{})
			Expect(err).ToNot(HaveOccurred())
			Expect(flexible).To(BeNil())
		})
		It("should read back the value it writes", func() {
			value, err := cloudprovider.FlexibleCapacityAnnotationValue(corev1.ResourceList{hugePages2Mi: resource.MustParse("8602Mi")})
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal(`{"hugepages-2Mi":"8602Mi"}`))

			flexible, err := cloudprovider.FlexibleCapacityFromNodeClaim(&v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.FlexibleCapacityAnnotationKey: value}},
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(flexible).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("8602Mi")))
		})
		It("should return an error for an invalid annotation", func() {
			_, err := cloudprovider.FlexibleCapacityFromNodeClaim(&v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.FlexibleCapacityAnnotationKey: "not json"}},
			})
			Expect(err).To(HaveOccurred())
		})
	})
	Context("ApplyFlexibleCapacity", func() {
		It("should set the chosen amounts and take hugepages out of allocatable memory", func() {
			capacity := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Gi")}
			allocatable := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("15Gi")}
			newCapacity, newAllocatable := cloudprovider.ApplyFlexibleCapacity(capacity, allocatable, corev1.ResourceList{
				hugePages2Mi:         resource.MustParse("2Gi"),
				"example.com/device": resource.MustParse("3"),
			})

			Expect(newCapacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("2Gi")))
			Expect(newCapacity.Memory().Value()).To(Equal(value("16Gi")))
			Expect(newAllocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("2Gi")))
			Expect(newAllocatable).To(HaveKeyWithValue(corev1.ResourceName("example.com/device"), resource.MustParse("3")))
			Expect(newAllocatable.Memory().Value()).To(Equal(value("13Gi")))
			// The maps passed in must not change
			Expect(capacity).ToNot(HaveKey(hugePages2Mi))
			Expect(allocatable.Memory().Value()).To(Equal(value("15Gi")))
		})
		It("should not take memory below zero", func() {
			_, allocatable := cloudprovider.ApplyFlexibleCapacity(nil,
				corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")},
			)
			Expect(allocatable.Memory().IsZero()).To(BeTrue())
		})
	})
	Context("FlexibleCapacityStatus", func() {
		var it *cloudprovider.InstanceType
		var baseMemory int64
		BeforeEach(func() {
			it = newFlexibleTestInstanceType()
			it.ApplyCapacityOverlay(corev1.ResourceList{"example.com/fixed": resource.MustParse("2")})
			it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")})
			baseMemory = memoryOf(it.Allocatable())
		})
		It("should replace the ceiling with the chosen amount when the provider reported the instance type as is", func() {
			capacity, allocatable := cloudprovider.FlexibleCapacityStatus(it, it.Capacity, it.Allocatable(), corev1.ResourceList{
				hugePages2Mi: resource.MustParse("2Gi"),
			})
			Expect(capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("2Gi")))
			Expect(capacity.Memory().Value()).To(Equal(value("16Gi")))
			Expect(allocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("2Gi")))
			Expect(allocatable.Memory().Value()).To(Equal(baseMemory - value("2Gi")))
			// Fixed resources keep their amount
			Expect(capacity).To(HaveKeyWithValue(corev1.ResourceName("example.com/fixed"), resource.MustParse("2")))
			Expect(allocatable).To(HaveKeyWithValue(corev1.ResourceName("example.com/fixed"), resource.MustParse("2")))
			// The instance type must not change
			Expect(it.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("8Gi")))
		})
		It("should not change a status that already has the chosen amounts", func() {
			flexible := corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")}
			capacity, allocatable := cloudprovider.ApplyFlexibleCapacity(
				lo.OmitByKeys(it.Capacity, []corev1.ResourceName{hugePages2Mi}),
				lo.OmitByKeys(it.Allocatable(), []corev1.ResourceName{hugePages2Mi}),
				flexible,
			)
			newCapacity, newAllocatable := cloudprovider.FlexibleCapacityStatus(it, capacity, allocatable, flexible)
			Expect(newCapacity).To(Equal(capacity))
			Expect(newAllocatable).To(Equal(allocatable))
			Expect(newAllocatable.Memory().Value()).To(Equal(baseMemory - value("2Gi")))
		})
		It("should remove a flexible resource with no chosen amount", func() {
			capacity, allocatable := cloudprovider.FlexibleCapacityStatus(it, it.Capacity, it.Allocatable(), nil)
			Expect(capacity).ToNot(HaveKey(hugePages2Mi))
			Expect(allocatable).ToNot(HaveKey(hugePages2Mi))
			Expect(allocatable.Memory().Value()).To(Equal(baseMemory))
		})
		It("should ignore a chosen amount for a resource that is not flexible", func() {
			capacity, allocatable := cloudprovider.FlexibleCapacityStatus(it, it.Capacity, it.Allocatable(), corev1.ResourceList{
				hugePages1Gi: resource.MustParse("2Gi"),
			})
			Expect(capacity).ToNot(HaveKey(hugePages1Gi))
			Expect(allocatable).ToNot(HaveKey(hugePages1Gi))
			Expect(allocatable.Memory().Value()).To(Equal(baseMemory))
		})
	})
})
