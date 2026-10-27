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
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
)

const hugePages2Mi corev1.ResourceName = "hugepages-2Mi"

func TestKWOKCloudProvider(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KWOK CloudProvider")
}

func quantityValue(s string) int64 {
	q := resource.MustParse(s)
	return q.Value()
}

var _ = Describe("toNode", func() {
	var provider *CloudProvider
	var nodeClaim *v1.NodeClaim
	BeforeEach(func() {
		provider = NewCloudProvider(context.Background(), nil, []*cloudprovider.InstanceType{
			fake.NewInstanceType("small", fake.WithResources(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				corev1.ResourcePods:   resource.MustParse("10"),
			})),
		})
		nodeClaim = &v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "nodeclaim", Labels: map[string]string{}},
			Spec: v1.NodeClaimSpec{
				Requirements: []v1.NodeSelectorRequirementWithMinValues{
					{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"small"}},
				},
				Resources: v1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("256Mi"),
					corev1.ResourcePods:   resource.MustParse("1"),
				}},
			},
		}
	})
	It("should give the node the chosen hugepages and take them out of allocatable memory", func() {
		nodeClaim.Spec.Resources.Requests[hugePages2Mi] = resource.MustParse("1Gi")
		nodeClaim.Annotations = map[string]string{v1.FlexibleCapacityAnnotationKey: `{"hugepages-2Mi":"1Gi"}`}

		node, err := provider.toNode(nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(node.Status.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("1Gi")))
		Expect(node.Status.Allocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("1Gi")))
		Expect(node.Status.Allocatable.Memory().Value()).To(Equal(quantityValue("4Gi") - quantityValue("10Mi") - quantityValue("1Gi")))
		Expect(node.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"1Gi"}`))
	})
	It("should not change the node shape without the annotation", func() {
		node, err := provider.toNode(nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(node.Status.Allocatable).ToNot(HaveKey(hugePages2Mi))
		Expect(node.Status.Allocatable.Memory().Value()).To(Equal(quantityValue("4Gi") - quantityValue("10Mi")))
		Expect(node.Status.Capacity.Memory().Value()).To(Equal(quantityValue("256Mi")))
	})
	It("should return an error for an invalid annotation", func() {
		nodeClaim.Annotations = map[string]string{v1.FlexibleCapacityAnnotationKey: "not json"}
		_, err := provider.toNode(nodeClaim)
		Expect(err).To(HaveOccurred())
	})
})
