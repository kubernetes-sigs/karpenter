package sharedcache_test

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/sharedcache"
)

func TestAllocatable(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "SharedCache")
}

var _ = Describe("Allocatable", func() {
	Context("UpdateAllocatable", func() {
		It("should correctly update allocatable regardless of precalculated values", func() {
			instanceType := &cloudprovider.InstanceType{
				Name:     "sample-instance",
				Capacity: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
				Overhead: &cloudprovider.InstanceTypeOverhead{
					KubeReserved:      corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")},
					SystemReserved:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("200Mi")},
					EvictionThreshold: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")},
				},
			}
			newAllocatable := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3123Mi")}
			instanceType.SetAllocatable(newAllocatable)
			expectedAllocatable := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3123Mi")}
			ExpectResources(newAllocatable, expectedAllocatable)
		})

		It("should learn true allocatable and prevent continuous node creation when resources are overestimated", func() {
			// Simulate overestimation by setting empty overhead
			instanceType := &cloudprovider.InstanceType{
				Name: "overestimated-instance",
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
					corev1.ResourcePods:   resource.MustParse("10"),
				},
				Overhead: &cloudprovider.InstanceTypeOverhead{},
				Offerings: []cloudprovider.Offering{
					{
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
						Price:        0.5,
						Available:    true,
					},
				},
			}

			nodePool := &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{Name: "test-nodepool"},
			}

			nodeClaimTemplate := &pscheduling.NodeClaimTemplate{
				NodePoolName:        nodePool.Name,
				InstanceTypeOptions: cloudprovider.InstanceTypes{instanceType},
				Requirements:        scheduling.NewRequirements(scheduling.NewRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpIn, nodePool.Name)),
			}

			// Create a pod that just fits the overestimated resources
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3.4"), corev1.ResourceMemory: resource.MustParse("15Gi")},
				},
			})

			// Attempt to schedule the pod
			firstNodeClaim := pscheduling.NewNodeClaim(nodeClaimTemplate, &pscheduling.Topology{}, corev1.ResourceList{}, []*cloudprovider.InstanceType{instanceType})
			err := firstNodeClaim.Add(pod)

			// Expect pod to be scheduled for the first time
			Expect(err).ShouldNot(HaveOccurred())

			// Simulate node creation and actual resource discovery
			// Update the shared cache with actual allocatable
			actualAllocatable := corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3"),
				corev1.ResourceMemory: resource.MustParse("14Gi"),
				corev1.ResourcePods:   resource.MustParse("10"),
			}

			cacheKey := fmt.Sprintf("allocatableCache;%s;%s", nodePool.Name, instanceType.Name)
			sharedcache.SharedCache().Set(cacheKey, actualAllocatable, sharedcache.DefaultSharedCacheTTL)

			// Should fail to schedule the pod again
			secondNodeClaim := pscheduling.NewNodeClaim(nodeClaimTemplate, &pscheduling.Topology{}, corev1.ResourceList{}, []*cloudprovider.InstanceType{instanceType})
			err = secondNodeClaim.Add(pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no instance type satisfied resources"))

			// Verify that the allocatable used was the actual allocatable, not the overestimated one
			cachedAllocatable, found := sharedcache.SharedCache().Get(cacheKey)
			Expect(found).To(BeTrue())
			ExpectResources(cachedAllocatable.(corev1.ResourceList), actualAllocatable)
		})

		It("should learn true allocatable resources when resources are underestimated", func() {
			// Simulate underestimation by increasing reserved to 6Gi
			instanceType := &cloudprovider.InstanceType{
				Name:     "underestimated-instance",
				Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi")},
				Overhead: &cloudprovider.InstanceTypeOverhead{
					KubeReserved:      corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
					SystemReserved:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
					EvictionThreshold: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
				},
				Offerings: []cloudprovider.Offering{
					{
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
						Price:        0.5,
						Available:    true,
					},
				},
			}
			nodePool := &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{Name: "test-nodepool"},
			}

			nodeClaimTemplate := &pscheduling.NodeClaimTemplate{
				NodePoolName:        nodePool.Name,
				InstanceTypeOptions: cloudprovider.InstanceTypes{instanceType},
				Requirements:        scheduling.NewRequirements(scheduling.NewRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpIn, nodePool.Name)),
			}

			// Create a pod that doesn't fit the underestimated resources but would fit the actual resources
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3.5"), corev1.ResourceMemory: resource.MustParse("14Gi")},
				},
			})

			// Initially, scheduling should fail
			firstNodeClaim := pscheduling.NewNodeClaim(nodeClaimTemplate, &pscheduling.Topology{}, corev1.ResourceList{}, []*cloudprovider.InstanceType{instanceType})
			err := firstNodeClaim.Add(pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no instance type satisfied resources"))

			// Simulate discovery of actual resources
			// Update the shared cache with actual allocatable
			actualAllocatable := corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3.8"),
				corev1.ResourceMemory: resource.MustParse("15Gi"),
				corev1.ResourcePods:   resource.MustParse("10"),
			}

			cacheMapKey := fmt.Sprintf("allocatableCache;%s;%s", nodePool.Name, instanceType.Name)
			sharedcache.SharedCache().Set(cacheMapKey, actualAllocatable, sharedcache.DefaultSharedCacheTTL)

			// Attempt to schedule the pod again
			secondNodeClaim := pscheduling.NewNodeClaim(nodeClaimTemplate, &pscheduling.Topology{}, corev1.ResourceList{}, []*cloudprovider.InstanceType{instanceType})
			err = secondNodeClaim.Add(pod)

			Expect(err).NotTo(HaveOccurred())
		})

	})
})
