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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Performance Benchmark", func() {
	Context("Provisioning", func() {
		It("should do simple provisioning", func() {
			deployment := test.Deployment(test.DeploymentOptions{
				Replicas: 100,
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
			start := time.Now()
			env.ExpectCreated(nodePool, nodeClass)
			env.EventuallyExpectHealthyPodCount(labelSelector, 100)
			// Need a way to respond to the last pod healthy event or look at the pod status conditions after the fact to get the exact measurements here.
			// would also be good to just pull the metrics directly from the Karpenter pod to get the scheduling simulation metrics.

			// env.Monitor.GetLastPodSchedulingEvent()
			duration := time.Since(start)

			fmt.Println("--------- RESULTS ---------")
			fmt.Printf("This is the duration: %s\n", duration)
		})
	})

})
