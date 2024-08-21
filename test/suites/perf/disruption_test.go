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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Disruption", func() {
	var replicas = 100
	It("should do simple provisioning and cloudprovider disruption", func() {
		deployment := test.Deployment(test.DeploymentOptions{
			Replicas: int32(replicas),
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: testLabels,
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"),
					},
				},
			}})
		env.ExpectCreated(deployment)
		env.ExpectCreated(nodePool, nodeClass)
		env.EventuallyExpectHealthyPodCount(labelSelector, replicas)

		env.TimeIntervalCollector.Start("KOWK Disruption")
		nodeClaimList := &v1.NodeClaimList{}
		Expect(env.Client.List(env, nodeClaimList, client.HasLabels{test.DiscoveryLabel})).To(Succeed())
		for i := range nodeClaimList.Items {
			Expect(nodeClaimList.Items[i].StatusConditions().SetTrue("ExampleReason")).To(BeTrue())
			Expect(env.Client.Status().Update(env, &nodeClaimList.Items[i])).To(Succeed())
		}

		// Eventually expect one node to have an ExampleReason
		Eventually(func(g Gomega) {
			nodeClaims := &v1.NodeClaimList{}
			g.Expect(env.Client.List(env, nodeClaims, client.MatchingFields{"status.conditions[*].type": "ExampleReason"})).To(Succeed())
			g.Expect(len(nodeClaims.Items)).ToNot(Equal(0))
		}).WithTimeout(5 * time.Second).Should(Succeed())
		// Then eventually expect no node to have an ExampleReason
		Eventually(func(g Gomega) {
			nodeClaims := &v1.NodeClaimList{}
			g.Expect(env.Client.List(env, nodeClaims, client.MatchingFields{"status.conditions[*].type": "ExampleReason"})).To(Succeed())
			g.Expect(len(nodeClaims.Items)).To(Equal(0))
		}).WithTimeout(3 * time.Minute).Should(Succeed())
		env.TimeIntervalCollector.End("KOWK Disruption")
	})
})
