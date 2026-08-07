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

package kwok

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
)

// A NodeClaim that never registers keeps whatever Status.Capacity toNode() gave it for its
// whole lifetime, since no kubelet ever corrects it. If that capacity only reflects the pod
// requests that triggered the NodeClaim rather than the selected instance type's real capacity,
// NodePool.Status.Resources and the scheduler's remaining-limits tracking both under-count the
// NodeClaim, letting Karpenter create far more NodeClaims than spec.limits should allow.
// https://github.com/kubernetes-sigs/karpenter/issues/2854
var _ = Describe("toNode", func() {
	It("should set node capacity from the selected instance type, not just the pod-derived resource requests", func() {
		instanceType := fake.NewInstanceType("big-instance-type", fake.WithResources(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("32"),
			corev1.ResourceMemory: resource.MustParse("128Gi"),
			corev1.ResourcePods:   resource.MustParse("100"),
		}))
		cp := CloudProvider{instanceTypes: []*cloudprovider.InstanceType{instanceType}}

		nodeClaim := test.NodeClaim(v1.NodeClaim{
			Spec: v1.NodeClaimSpec{
				Requirements: []v1.NodeSelectorRequirementWithMinValues{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{instanceType.Name},
					},
				},
				Resources: v1.ResourceRequirements{
					// Mimics a single small pod's request, which is what the NodeClaim
					// controller sizes Spec.Resources.Requests to.
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("0.6"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			},
		})

		node, err := cp.toNode(nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(node.Status.Capacity.Cpu().Value()).To(BeEquivalentTo(32))
		expectedMemory := resource.MustParse("128Gi")
		Expect(node.Status.Capacity.Memory().Value()).To(BeEquivalentTo(expectedMemory.Value()))
	})
})
