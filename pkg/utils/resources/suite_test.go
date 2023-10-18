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

package resources

import (
	"sigs.k8s.io/karpenter/pkg/test"

	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestResources(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Resources Suite")
}

var _ = Describe("Resources Handling", func() {
	Context("Resource Calculations", func() {

		It("should schedule based on the sum of containers and sidecarContainers", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Limits.Cpu().CmpInt64(6)).To(Equal(0))
			Expect(podResources.Limits.Memory().CmpInt64(3221225472)).To(Equal(0))
		})

		It("should schedule based on the sum of containers , sidecarContainers and overhead with max of initcontainers", func() {
			pod := test.Pod(test.PodOptions{
				Overhead: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("5"),
					v1.ResourceMemory: resource.MustParse("500Mi"),
				},
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},

				InitContainers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Limits.Cpu().CmpInt64(10)).To(Equal(0))
			Expect(podResources.Limits.Memory().CmpInt64(4819255296)).To(Equal(0))
		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers for order of type1", func() {

			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("10"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Limits.Cpu().CmpInt64(14)).To(Equal(0))
			Expect(podResources.Limits.Memory().CmpInt64(4294967296)).To(Equal(0))

		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers for order of type2", func() {

			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Limits.Cpu().CmpInt64(6)).To(Equal(0))
			Expect(podResources.Limits.Memory().CmpInt64(4294967296)).To(Equal(0))

		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers for order of type3", func() {

			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("20"), v1.ResourceMemory: resource.MustParse("20Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Limits.Cpu().CmpInt64(31)).To(Equal(0))
			Expect(podResources.Limits.Memory().CmpInt64(33285996544)).To(Equal(0))

		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers for order of type4", func() {

			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Limits.Cpu().CmpInt64(14)).To(Equal(0))
			Expect(podResources.Limits.Memory().CmpInt64(15032385536)).To(Equal(0))

		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers for order of type5", func() {

			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},

				InitContainers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("25"), v1.ResourceMemory: resource.MustParse("25Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},

					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Limits.Cpu().CmpInt64(25)).To(Equal(0))
			Expect(podResources.Limits.Memory().CmpInt64(26843545600)).To(Equal(0))

		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers for order of type6", func() {

			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},

				InitContainers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},

					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Limits.Cpu().CmpInt64(10)).To(Equal(0))
			Expect(podResources.Limits.Memory().CmpInt64(10737418240)).To(Equal(0))

		})

	})

})
