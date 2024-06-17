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

package perf_test

import (
	. "github.com/onsi/ginkgo/v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/test"
)

var replicas int = 100

var _ = Describe("Performance", func() {
	Context("Provisioning", func() {
		It("should do simple provisioning", func() {
			deployment := test.Deployment(test.DeploymentOptions{
				Replicas: int32(replicas),
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: testLabels,
					},
					ResourceRequirements: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				}})
			env.ExpectCreated(deployment)
			env.ExpectCreated(nodePool, nodeClass)
			env.EventuallyExpectHealthyPodCount(labelSelector, replicas)
		})
		It("should do complex provisioning", func() {
			deployments := []*appsv1.Deployment{}
			for _, option := range test.MakeDiversePodOptions() {
				deployments = append(deployments, test.Deployment(
					test.DeploymentOptions{
						PodOptions: option,
						Replicas:   int32(replicas),
					},
				))
			}
			for _, dep := range deployments {
				env.ExpectCreated(dep)
			}
			env.TimeIntervalCollector.Start("PostDeployment")
			defer env.TimeIntervalCollector.End("PostDeployment")

			env.ExpectCreated(nodePool, nodeClass)
			env.EventuallyExpectHealthyPodCount(labelSelector, len(deployments)*replicas)
		})
	})
})
