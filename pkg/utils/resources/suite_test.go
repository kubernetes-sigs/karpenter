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
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Requests.Cpu().Value()).To(BeNumerically("==", 3))
			Expect(podResources.Requests.Memory().Value()).To(BeNumerically("==", 3221225472))
			Expect(podResources.Limits.Cpu().Value()).To(BeNumerically("==", 6))
			Expect(podResources.Limits.Memory().Value()).To(BeNumerically("==", 3221225472))
		})

		It("should schedule based on the sum of containers , sidecarContainers and overhead with max of initcontainers", func() {
			pod := test.Pod(test.PodOptions{
				Overhead: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("5"),
					v1.ResourceMemory: resource.MustParse("500Mi"),
				},
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
				},

				InitContainers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Requests.Cpu().Value()).To(BeNumerically("==", 8))
			Expect(podResources.Requests.Memory().Value()).To(BeNumerically("==", 4819255296))
			Expect(podResources.Limits.Cpu().Value()).To(BeNumerically("==", 10))
			Expect(podResources.Limits.Memory().Value()).To(BeNumerically("==", 4819255296))
		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers when there is a large initcontainer", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("10"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Requests.Cpu().Value()).To(BeNumerically("==", 3))
			Expect(podResources.Requests.Memory().Value()).To(BeNumerically("==", 4294967296))
			Expect(podResources.Limits.Cpu().Value()).To(BeNumerically("==", 14))
			Expect(podResources.Limits.Memory().Value()).To(BeNumerically("==", 4294967296))
		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers when there is a large sidecar container", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			Expect(podResources.Requests.Cpu().Value()).To(BeNumerically("==", 3))
			Expect(podResources.Requests.Memory().Value()).To(BeNumerically("==", 4294967296))
			Expect(podResources.Limits.Cpu().Value()).To(BeNumerically("==", 6))
			Expect(podResources.Limits.Memory().Value()).To(BeNumerically("==", 4294967296))
		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers when considering multiple sidecars and a large initcontainer", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("3Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
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
			Expect(podResources.Requests.Cpu().Value()).To(BeNumerically("==", 6))
			Expect(podResources.Requests.Memory().Value()).To(BeNumerically("==", 33285996544))
			Expect(podResources.Limits.Cpu().Value()).To(BeNumerically("==", 31))
			Expect(podResources.Limits.Memory().Value()).To(BeNumerically("==", 33285996544))
		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers for multiple sidecars and a small initcontainer", func() {
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
			Expect(podResources.Limits.Cpu().Value()).To(BeNumerically("==", 14))
			Expect(podResources.Limits.Memory().Value()).To(BeNumerically("==", 15032385536))
		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers with multiple sidecars and initcontainers where one of the initcontainer exceeds the sum of them", func() {
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
			Expect(podResources.Requests.Cpu().Value()).To(BeNumerically("==", 5))
			Expect(podResources.Requests.Memory().Value()).To(BeNumerically("==", 26843545600))
			Expect(podResources.Limits.Cpu().Value()).To(BeNumerically("==", 25))
			Expect(podResources.Limits.Memory().Value()).To(BeNumerically("==", 26843545600))
		})

		It("should schedule based on the max resource requests of containers and initContainers with sidecarContainers with multiple sidecars and initcontainers where one of the initcontainer does not exceed the sum of them", func() {
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
			Expect(podResources.Requests.Cpu().Value()).To(BeNumerically("==", 5))
			Expect(podResources.Requests.Memory().Value()).To(BeNumerically("==", 10737418240))
			Expect(podResources.Limits.Cpu().Value()).To(BeNumerically("==", 10))
			Expect(podResources.Limits.Memory().Value()).To(BeNumerically("==", 10737418240))
		})
	})
})
