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

package capacitybuffer

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

var _ = Describe("Helpers", func() {
	Context("calculateLimitReplicas", func() {
		It("should calculate replicas based on CPU limits", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				}},
			}
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			}, podSpec)
			Expect(result).To(Equal(int32(4)))
		})

		It("should calculate replicas based on memory limits", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				}},
			}
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("2Gi"),
			}, podSpec)
			Expect(result).To(Equal(int32(4)))
		})

		It("should take the minimum across multiple resources", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("1"),
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				}},
			}
			// 8 CPUs / 1 CPU = 8, but 4Gi / 1Gi = 4 → min is 4
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("4Gi"),
			}, podSpec)
			Expect(result).To(Equal(int32(4)))
		})

		It("should sum requests across multiple containers", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
					}},
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
					}},
				},
			}
			// Total pod request = 1 CPU, limit = 3 CPU → 3 replicas
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("3"),
			}, podSpec)
			Expect(result).To(Equal(int32(3)))
		})

		It("should use init container max if larger than sum of containers", func() {
			podSpec := &v1.PodSpec{
				InitContainers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4")},
					},
				}},
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					},
				}},
			}
			// Init container needs 4 CPU > regular container 1 CPU → effective = 4 CPU
			// 8 CPU limit / 4 CPU per pod = 2
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("8"),
			}, podSpec)
			Expect(result).To(Equal(int32(2)))
		})

		It("should use container sum if larger than init containers", func() {
			podSpec := &v1.PodSpec{
				InitContainers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
					},
				}},
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
					},
				}},
			}
			// Container needs 2 CPU > init container 500m → effective = 2 CPU
			// 6 CPU limit / 2 CPU per pod = 3
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("6"),
			}, podSpec)
			Expect(result).To(Equal(int32(3)))
		})

		It("should return false when pod has no resource requests", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{Name: "app"}},
			}
			_, matched := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			}, podSpec)
			Expect(matched).To(BeFalse())
		})

		It("should return false when no limit resources match pod requests", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					},
				}},
			}
			_, matched := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("4Gi"),
			}, podSpec)
			Expect(matched).To(BeFalse())
		})

		It("should floor divide (no rounding up)", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3")},
					},
				}},
			}
			// 10 / 3 = 3.33 → floor = 3
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("10"),
			}, podSpec)
			Expect(result).To(Equal(int32(3)))
		})

		It("should handle millicore-level precision", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m")},
					},
				}},
			}
			// 1000m / 250m = 4
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("1"),
			}, podSpec)
			Expect(result).To(Equal(int32(4)))
		})

		It("should return 0 when limit is smaller than a single pod request", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
					},
				}},
			}
			// 1 / 2 = 0
			result, _ := calculateLimitReplicas(v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("1"),
			}, podSpec)
			Expect(result).To(Equal(int32(0)))
		})
	})

	Context("totalPodRequests", func() {
		It("should sum requests from all containers", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("1"),
							v1.ResourceMemory: resource.MustParse("512Mi"),
						},
					}},
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("500m"),
							v1.ResourceMemory: resource.MustParse("256Mi"),
						},
					}},
				},
			}
			total := resources.RequestsForSpec(podSpec)
			cpu := total[v1.ResourceCPU]
			Expect(cpu.MilliValue()).To(Equal(int64(1500)))
			mem := total[v1.ResourceMemory]
			Expect(mem.Value()).To(Equal(int64(768 * 1024 * 1024)))
		})

		It("should take max of init containers vs regular containers", func() {
			podSpec := &v1.PodSpec{
				InitContainers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4")},
					}},
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
					}},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					}},
				},
			}
			total := resources.RequestsForSpec(podSpec)
			cpu := total[v1.ResourceCPU]
			Expect(cpu.MilliValue()).To(Equal(int64(4000)))
		})

		It("should return empty list for pod with no requests", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{{Name: "app"}},
			}
			total := resources.RequestsForSpec(podSpec)
			Expect(total).To(BeEmpty())
		})

		It("should handle mixed resources across containers", func() {
			podSpec := &v1.PodSpec{
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					}},
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("1Gi")},
					}},
				},
			}
			total := resources.RequestsForSpec(podSpec)
			cpu := total[v1.ResourceCPU]
			mem := total[v1.ResourceMemory]
			Expect(cpu.MilliValue()).To(Equal(int64(1000)))
			Expect(mem.Value()).To(Equal(int64(1024 * 1024 * 1024)))
		})

		It("should sum sidecar init containers with regular containers", func() {
			always := v1.ContainerRestartPolicyAlways
			podSpec := &v1.PodSpec{
				InitContainers: []v1.Container{
					{
						RestartPolicy: &always,
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
						},
					},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					}},
				},
			}
			// Sidecar (500m) + regular (1) = 1500m
			total := resources.RequestsForSpec(podSpec)
			cpu := total[v1.ResourceCPU]
			Expect(cpu.MilliValue()).To(Equal(int64(1500)))
		})

		It("should sum multiple sidecar init containers with regular containers", func() {
			always := v1.ContainerRestartPolicyAlways
			podSpec := &v1.PodSpec{
				InitContainers: []v1.Container{
					{
						RestartPolicy: &always,
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
						},
					},
					{
						RestartPolicy: &always,
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					}},
				},
			}
			// Sidecars (500m + 200m) + regular (1) = 1700m
			total := resources.RequestsForSpec(podSpec)
			cpu := total[v1.ResourceCPU]
			Expect(cpu.MilliValue()).To(Equal(int64(1700)))
		})

		It("should use max of regular init container + preceding sidecars vs running total", func() {
			always := v1.ContainerRestartPolicyAlways
			podSpec := &v1.PodSpec{
				InitContainers: []v1.Container{
					{
						RestartPolicy: &always,
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
						},
					},
					{
						// Regular init container: initUse = 4 CPU + 500m sidecar = 4500m
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4")},
						},
					},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					}},
				},
			}
			// reqs = regular (1) + sidecar (500m) = 1500m
			// initUse = regular init (4) + sidecar (500m) = 4500m
			// effective = max(1500m, 4500m) = 4500m
			total := resources.RequestsForSpec(podSpec)
			cpu := total[v1.ResourceCPU]
			Expect(cpu.MilliValue()).To(Equal(int64(4500)))
		})

		It("should handle sidecar after regular init container", func() {
			always := v1.ContainerRestartPolicyAlways
			podSpec := &v1.PodSpec{
				InitContainers: []v1.Container{
					{
						// Regular init runs first alone: initUse = 4 CPU
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4")},
						},
					},
					{
						// Sidecar starts after: summed with regular containers
						RestartPolicy: &always,
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
						},
					},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					}},
				},
			}
			// reqs = regular (1) + sidecar (500m) = 1500m
			// initUse = regular init (4) + no preceding sidecars = 4000m
			// effective = max(1500m, 4000m) = 4000m
			total := resources.RequestsForSpec(podSpec)
			cpu := total[v1.ResourceCPU]
			Expect(cpu.MilliValue()).To(Equal(int64(4000)))
		})

		It("should not treat init containers without restartPolicy as sidecars", func() {
			podSpec := &v1.PodSpec{
				InitContainers: []v1.Container{
					{
						// No RestartPolicy — regular init container
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
						},
					},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					}},
				},
			}
			// Regular init (2) > regular container (1) → effective = 2 CPU
			total := resources.RequestsForSpec(podSpec)
			cpu := total[v1.ResourceCPU]
			Expect(cpu.MilliValue()).To(Equal(int64(2000)))
		})
	})

	Context("calculatePercentageReplicas", func() {
		It("should calculate percentage and ceil", func() {
			// 20% of 10 = 2
			Expect(calculatePercentageReplicas(10, 20)).To(Equal(int32(2)))
		})

		It("should ceil fractional results", func() {
			// 10% of 3 = 0.3, ceil = 1
			Expect(calculatePercentageReplicas(3, 10)).To(Equal(int32(1)))
		})

		It("should return at least 1 when percentage > 0 and replicas > 0", func() {
			// 1% of 1 = 0.01, ceil = 1
			Expect(calculatePercentageReplicas(1, 1)).To(Equal(int32(1)))
		})

		It("should return 0 when scalable has 0 replicas", func() {
			Expect(calculatePercentageReplicas(0, 50)).To(Equal(int32(0)))
		})

		It("should return 0 when percentage is 0", func() {
			Expect(calculatePercentageReplicas(10, 0)).To(Equal(int32(0)))
		})

		It("should handle 100 percent", func() {
			Expect(calculatePercentageReplicas(5, 100)).To(Equal(int32(5)))
		})

		It("should handle large percentages (>100)", func() {
			// 200% of 5 = 10
			Expect(calculatePercentageReplicas(5, 200)).To(Equal(int32(10)))
		})

		It("should handle large replica counts", func() {
			// 10% of 1000 = 100
			Expect(calculatePercentageReplicas(1000, 10)).To(Equal(int32(100)))
		})
	})

	Context("SetCondition", func() {
		It("should add a new condition", func() {
			cb := &autoscalingv1beta1.CapacityBuffer{}
			cb.SetCondition("TestCond", metav1.ConditionTrue, "TestReason", "test message")

			Expect(cb.Status.Conditions).To(HaveLen(1))
			Expect(cb.Status.Conditions[0].Type).To(Equal("TestCond"))
			Expect(cb.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(cb.Status.Conditions[0].Reason).To(Equal("TestReason"))
			Expect(cb.Status.Conditions[0].Message).To(Equal("test message"))
		})

		It("should update an existing condition", func() {
			cb := &autoscalingv1beta1.CapacityBuffer{}
			cb.SetCondition("TestCond", metav1.ConditionFalse, "FirstReason", "first")
			cb.SetCondition("TestCond", metav1.ConditionTrue, "SecondReason", "second")

			Expect(cb.Status.Conditions).To(HaveLen(1))
			Expect(cb.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(cb.Status.Conditions[0].Reason).To(Equal("SecondReason"))
			Expect(cb.Status.Conditions[0].Message).To(Equal("second"))
		})

		It("should support multiple condition types", func() {
			cb := &autoscalingv1beta1.CapacityBuffer{}
			cb.SetCondition("CondA", metav1.ConditionTrue, "A", "a")
			cb.SetCondition("CondB", metav1.ConditionFalse, "B", "b")

			Expect(cb.Status.Conditions).To(HaveLen(2))
		})

		It("should set observed generation", func() {
			cb := &autoscalingv1beta1.CapacityBuffer{}
			cb.Generation = 7
			cb.SetCondition("TestCond", metav1.ConditionTrue, "R", "m")

			Expect(cb.Status.Conditions[0].ObservedGeneration).To(Equal(int64(7)))
		})
	})
})
