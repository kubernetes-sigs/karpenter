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

package scheduling

import (
	"unique"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

var _ = Describe("DRA Scheduling Internals", func() {
	Describe("resourceClaimName", func() {
		It("should use a direct ResourceClaimName", func() {
			pod := &corev1.Pod{}
			claim := corev1.PodResourceClaim{Name: "ref", ResourceClaimName: lo.ToPtr("claim-a")}
			name, ok := resourceClaimName(pod, &claim)
			Expect(ok).To(BeTrue())
			Expect(name).To(Equal("claim-a"))
		})
		It("should resolve a template's generated name from pod status", func() {
			pod := &corev1.Pod{Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{
				{Name: "ref", ResourceClaimName: lo.ToPtr("generated-a")},
			}}}
			claim := corev1.PodResourceClaim{Name: "ref"}
			name, ok := resourceClaimName(pod, &claim)
			Expect(ok).To(BeTrue())
			Expect(name).To(Equal("generated-a"))
		})
		It("should skip a template whose generated name is nil", func() {
			pod := &corev1.Pod{Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{
				{Name: "ref", ResourceClaimName: nil},
			}}}
			claim := corev1.PodResourceClaim{Name: "ref"}
			_, ok := resourceClaimName(pod, &claim)
			Expect(ok).To(BeFalse())
		})
		It("should skip when no matching status entry exists", func() {
			pod := &corev1.Pod{}
			claim := corev1.PodResourceClaim{Name: "ref"}
			_, ok := resourceClaimName(pod, &claim)
			Expect(ok).To(BeFalse())
		})
	})

	Describe("draNodeClaim adapter", func() {
		var it1, it2 *cloudprovider.InstanceType
		var nc *NodeClaim

		BeforeEach(func() {
			it1 = &cloudprovider.InstanceType{
				Name: "it-1",
				DynamicResources: cloudprovider.DynamicResources{
					ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-a")}},
				},
			}
			it2 = &cloudprovider.InstanceType{Name: "it-2"}
			nc = &NodeClaim{hostname: "hostname-1"}
			nc.NodePoolName = "np-a"
			nc.Requirements = scheduling.NewRequirements()
			nc.InstanceTypeOptions = cloudprovider.InstanceTypes{it1, it2}
		})

		It("should map identity, nodepool, and instance types", func() {
			adapter := &draNodeClaim{nc: nc}
			Expect(adapter.ID()).To(Equal(unique.Make("hostname-1")))
			Expect(adapter.NodePoolID()).To(Equal(unique.Make("np-a")))
			its := lo.Map(adapter.InstanceTypes(), func(id unique.Handle[string], _ int) string { return id.Value() })
			Expect(its).To(ConsistOf("it-1", "it-2"))
		})
		It("should expose template slices per instance type", func() {
			adapter := &draNodeClaim{nc: nc}
			slices := adapter.ResourceSlices()
			Expect(slices[unique.Make("it-1")]).To(HaveLen(1))
			Expect(slices[unique.Make("it-2")]).To(BeEmpty())
		})
	})

	Describe("draExistingNode adapter", func() {
		var it *cloudprovider.InstanceType

		BeforeEach(func() {
			it = &cloudprovider.InstanceType{
				Name: "it-1",
				DynamicResources: cloudprovider.DynamicResources{
					ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-a")}},
				},
			}
		})

		It("should return no template slices for an initialized node", func() {
			adapter := draExistingNodeForTest(true, it)
			Expect(adapter.ResourceSlices()).To(BeEmpty())
		})
		It("should return the full template set for a pre-initialized node", func() {
			adapter := draExistingNodeForTest(false, it)
			Expect(adapter.ResourceSlices()[unique.Make("it-1")]).To(HaveLen(1))
		})
		It("should return no template slices for a pre-initialized node with an unresolved instance type", func() {
			adapter := draExistingNodeForTest(false, nil)
			Expect(adapter.ResourceSlices()).To(BeEmpty())
		})
	})
})

// draExistingNodeForTest builds a draExistingNode backed by a StateNode whose initialization state and labels are set
// for exercising the adapter's ResourceSlices() behavior.
func draExistingNodeForTest(initialized bool, it *cloudprovider.InstanceType) *draExistingNode {
	labels := map[string]string{
		corev1.LabelInstanceTypeStable: "it-1",
		v1.NodePoolLabelKey:            "np-a",
	}
	if initialized {
		labels[v1.NodeInitializedLabelKey] = "true"
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: labels}}
	nodeClaim := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
	en := &ExistingNode{StateNode: &state.StateNode{Node: node, NodeClaim: nodeClaim}, instanceType: it}
	en.requirements = scheduling.NewLabelRequirements(labels)
	return &draExistingNode{en: en, instanceType: it}
}
