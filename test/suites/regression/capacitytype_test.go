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

package integration_test

import (
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("CapacityTypeClasses", func() {
	var selector labels.Selector

	BeforeEach(func() {
		if !env.IsDefaultNodeClassKWOK() {
			Skip("the on-demand class capacity type under test is registered by the KWOK provider")
		}
		test.ReplaceRequirements(nodePool, v1.NodeSelectorRequirementWithMinValues{
			Key:      v1.CapacityTypeLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{kwok.CapacityTypeOnDemandAlt},
		})
		selector = labels.SelectorFromSet(map[string]string{"app": "capacity-type-class"})
	})

	deployment := func(replicas int32, cpu string) *test.DeploymentOptions {
		return &test.DeploymentOptions{
			Replicas: replicas,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "capacity-type-class"}},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
				},
			},
		}
	}

	It("should launch a node for a NodePool that requires a provider-defined capacity type", func() {
		dep := test.Deployment(*deployment(1, "1"))
		env.ExpectCreated(nodeClass, nodePool, dep)

		env.EventuallyExpectHealthyPodCount(selector, 1)
		node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		Expect(node.Labels).To(HaveKeyWithValue(v1.CapacityTypeLabelKey, kwok.CapacityTypeOnDemandAlt))
		Expect(nodeClaim.Labels).To(HaveKeyWithValue(v1.CapacityTypeLabelKey, kwok.CapacityTypeOnDemandAlt))
		env.EventuallyExpectNodeClaimsReady(nodeClaim)
	})

	It("should replace an underutilized node with a cheaper node of the same capacity type", func() {
		nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("Never")
		dep := test.Deployment(*deployment(2, "3"))
		env.ExpectCreated(nodeClass, nodePool, dep)

		env.EventuallyExpectHealthyPodCount(selector, 2)
		original := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		originalCPU := original.Status.Capacity[corev1.ResourceCPU]

		dep.Spec.Replicas = lo.ToPtr[int32](1)
		env.ExpectUpdated(dep)
		env.EventuallyExpectHealthyPodCount(selector, 1)
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
		env.ExpectUpdated(nodePool)

		env.EventuallyExpectNotFound(original)
		replacement := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		Expect(replacement.Labels).To(HaveKeyWithValue(v1.CapacityTypeLabelKey, kwok.CapacityTypeOnDemandAlt))
		replacementCPU := replacement.Status.Capacity[corev1.ResourceCPU]
		Expect(replacementCPU.Cmp(originalCPU)).To(Equal(-1))
		env.EventuallyExpectHealthyPodCount(selector, 1)
	})
})
