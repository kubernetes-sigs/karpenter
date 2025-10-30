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

package performance

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

var _ = Describe("Consolidation", func() {
	BeforeEach(func() {
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
	})
	It("should consolidate nodes after the workload is scaled down", func() {
		numPods := 10
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: int32(numPods),
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
		// Hostname anti-affinity to require one pod on each node
		dep.Spec.Template.Spec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{
						LabelSelector: dep.Spec.Selector,
						TopologyKey:   corev1.LabelHostname,
					},
				},
			},
		}
		env.ExpectCreated(nodeClass, nodePool, dep)

		env.EventuallyExpectCreatedNodeClaimCount("==", numPods)
		nodes := env.EventuallyExpectCreatedNodeCount("==", numPods)
		Expect(env.GetClusterCost()).To(BeNumerically(">", 0.0))

		env.EventuallyExpectHealthyPodCount(labelSelector, numPods)

		By("adding finalizers to the nodes to prevent termination")
		for _, node := range nodes {
			Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(node), node)).To(Succeed())
			node.Finalizers = append(node.Finalizers, common.TestingFinalizer)
			env.ExpectUpdated(node)
		}

		dep.Spec.Replicas = lo.ToPtr[int32](1)
		By("making the nodes empty")
		// Update the deployment to only contain 1 replica.
		env.ExpectUpdated(dep)

		env.ConsistentlyExpectDisruptionsUntilNoneLeft(numPods, numPods, 2*time.Minute)
	})
})
