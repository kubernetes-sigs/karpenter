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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/test"
)

// ExpectResources expects all the resources in expected to exist in real with the same values
func ExpectResources(expected, real v1.ResourceList) {
	GinkgoHelper()
	for k, v := range expected {
		realV := real[k]
		Expect(v.Value()).To(BeNumerically("~", realV.Value()))
	}
}

func TestResources(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Resources Suite")
}

var _ = Describe("Resources Handling", func() {
	Context("Resource Calculations", func() {
		It("should calculate resource requests based off of the sum of containers and sidecarContainers", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("3"),
				v1.ResourceMemory: resource.MustParse("3Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("3"),
				v1.ResourceMemory: resource.MustParse("3Gi"),
			})
		})
		It("should calculate resource requests based off of containers, sidecarContainers, initContainers, and overhead", func() {
			pod := test.Pod(test.PodOptions{
				Overhead: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("5"),
					v1.ResourceMemory: resource.MustParse("1Gi"),
				},
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
				},

				InitContainers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("5Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("5Gi"),
			})
		})
		It("should calculate resource requests when there is a large initContainer after a sidecarContainer that exceeds container resource requests", func() {
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
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("10"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("14"),
				v1.ResourceMemory: resource.MustParse("4Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("14"),
				v1.ResourceMemory: resource.MustParse("4Gi"),
			})
		})
		It("should calculate resource requests when there is an initContainer after a sidecarContainer that doesn't exceed container resource requests", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("6"),
				v1.ResourceMemory: resource.MustParse("4Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("6"),
				v1.ResourceMemory: resource.MustParse("4Gi"),
			})
		})
		It("should calculate resource requests when a single sidecarContainer is bigger than the container resource requests", func() {
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
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("2Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("6"),
				v1.ResourceMemory: resource.MustParse("3Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("6"),
				v1.ResourceMemory: resource.MustParse("3Gi"),
			})
		})
		It("should calculate resource requests when a single sidecarContainer is not bigger than container resource requests", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("3"),
				v1.ResourceMemory: resource.MustParse("3Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("3"),
				v1.ResourceMemory: resource.MustParse("3Gi"),
			})
		})
		It("should calculate resource requests when an initContainer comes after multiple sidecarContainers that exceeds container resource requests", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
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
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("20"), v1.ResourceMemory: resource.MustParse("20Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("20"), v1.ResourceMemory: resource.MustParse("20Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("31"),
				v1.ResourceMemory: resource.MustParse("31Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("31"),
				v1.ResourceMemory: resource.MustParse("31Gi"),
			})
		})
		It("should calculate resource requests when an initContainer comes after multiple sidecarContainers that doesn't exceed container resource requests", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
				},
				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
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
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("14"),
				v1.ResourceMemory: resource.MustParse("14Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("14"),
				v1.ResourceMemory: resource.MustParse("14Gi"),
			})
		})
		It("should calculate resource requests when an initContainer comes after multiple sidecarContainers that exceeds container resource requests", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
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
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("15"), v1.ResourceMemory: resource.MustParse("15Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("15"), v1.ResourceMemory: resource.MustParse("15Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("26"),
				v1.ResourceMemory: resource.MustParse("26Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("26"),
				v1.ResourceMemory: resource.MustParse("26Gi"),
			})
		})
		It("should calculate resource requests with multiple sidecarContainers when one initContainer exceeds the sum of all sidecarContainers and container resource requests", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
				},

				InitContainers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("25"), v1.ResourceMemory: resource.MustParse("25Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("25"), v1.ResourceMemory: resource.MustParse("25Gi")},
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
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},

					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("25"),
				v1.ResourceMemory: resource.MustParse("25Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("25"),
				v1.ResourceMemory: resource.MustParse("25Gi"),
			})
		})
		It("should calculate resource requests with multiple sidecarContainers and initContainers when one initContainer exceeds the sum of all sidecarContainers and container resource requests", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
				},

				InitContainers: []v1.Container{
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("25"), v1.ResourceMemory: resource.MustParse("25Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("25"), v1.ResourceMemory: resource.MustParse("25Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},

					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("26"),
				v1.ResourceMemory: resource.MustParse("26Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("26"),
				v1.ResourceMemory: resource.MustParse("26Gi"),
			})
		})
		It("should calculate resource requests with multiple sidecarContainers and initContainers when there are multiple initContainers interspersed with sidecarContainers", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
				},

				InitContainers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
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
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},

					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
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
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("10Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("10Gi"),
			})
		})
		It("should calculate resource requests with multiple sidecarContainers and initContainers when there are multiple initContainers interspersed with initContainers", func() {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
				},

				InitContainers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")},
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
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("3Gi")},
						},
					},

					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("5Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
					{
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
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
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
						},
					},
				},
			})
			podResources := Ceiling(pod)
			ExpectResources(podResources.Requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("10Gi"),
			})
			ExpectResources(podResources.Limits, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("10Gi"),
			})
		})
	})
	Context("Resource Merging", func() {
		It("should merge resource limits into requests if no request exists for the given container", func() {
			container := v1.Container{
				Resources: v1.ResourceRequirements{
					Limits: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			}
			requests := MergeResourceLimitsIntoRequests(container)
			ExpectResources(requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			})
		})
		It("should merge resource limits into requests if no request exists for the given sidecarContainer", func() {
			container := v1.Container{
				RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways),
				Resources: v1.ResourceRequirements{
					Limits: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			}
			requests := MergeResourceLimitsIntoRequests(container)
			ExpectResources(requests, v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			})
		})
	})
})
