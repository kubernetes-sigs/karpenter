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
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

var _ = Describe("DRA Scheduling Internals", func() {
	Describe("resourceClaimName", func() {
		It("should use a direct ResourceClaimName", func() {
			pod := &corev1.Pod{}
			claim := corev1.PodResourceClaim{Name: "ref", ResourceClaimName: new("claim-a")}
			name, ok := resourceClaimName(pod, &claim)
			Expect(ok).To(BeTrue())
			Expect(name).To(Equal("claim-a"))
		})
		It("should resolve a template's generated name from pod status", func() {
			pod := &corev1.Pod{Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{
				{Name: "ref", ResourceClaimName: new("generated-a")},
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
				DynamicResources: []*cloudprovider.DynamicResources{{
					ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-a")}},
				}},
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
		It("should be superposed", func() {
			Expect((&draNodeClaim{nc: nc}).Superposed()).To(BeTrue())
		})
	})

	Describe("requirement-scoped DynamicResources sets", func() {
		const modeKey = "example.com/driver-mode-gpu"
		var it *cloudprovider.InstanceType
		var nc *NodeClaim

		offering := func(zone, mode string, available bool) *cloudprovider.Offering {
			return &cloudprovider.Offering{
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
					scheduling.NewRequirement(modeKey, corev1.NodeSelectorOpIn, mode),
				),
				Available: available,
			}
		}
		modeReqs := func(mode string) scheduling.Requirements {
			return scheduling.NewRequirements(scheduling.NewRequirement(modeKey, corev1.NodeSelectorOpIn, mode))
		}
		slicesFor := func(adapter interface {
			ResourceSlices() map[unique.Handle[string]][]dynamicresources.ResourceSlice
		}) []string {
			return lo.Map(adapter.ResourceSlices()[unique.Make("it-1")], func(s dynamicresources.ResourceSlice, _ int) string {
				return s.Pool().Name.Value()
			})
		}

		BeforeEach(func() {
			v1.WellKnownLabels.Insert(modeKey)
			DeferCleanup(func() { v1.WellKnownLabels.Delete(modeKey) })
			it = &cloudprovider.InstanceType{
				Name: "it-1",
				Offerings: cloudprovider.Offerings{
					offering("zone-a", "device-plugin", true),
					offering("zone-a", "dra", false),
					offering("zone-b", "dra", true),
					offering("zone-a", "other", false),
				},
				DynamicResources: []*cloudprovider.DynamicResources{
					{ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-a"), Pool: cloudprovider.ResourcePool{Name: unique.Make("unscoped")}}}},
					{Requirements: modeReqs("dra"), ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-b"), Pool: cloudprovider.ResourcePool{Name: unique.Make("dra")}}}},
					{Requirements: modeReqs("other"), ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-b"), Pool: cloudprovider.ResourcePool{Name: unique.Make("other")}}}},
				},
			}
			nc = &NodeClaim{hostname: "hostname-1"}
			nc.NodePoolName = "np-a"
			nc.Requirements = scheduling.NewRequirements()
			nc.InstanceTypeOptions = cloudprovider.InstanceTypes{it}
		})

		It("should expose sets with an available offering that are compatible with the NodeClaim", func() {
			Expect(slicesFor(&draNodeClaim{nc: nc})).To(Equal([]string{"unscoped", "dra"}))
		})
		It("should exclude sets that are incompatible with the NodeClaim's requirements", func() {
			nc.Requirements = modeReqs("device-plugin")
			Expect(slicesFor(&draNodeClaim{nc: nc})).To(Equal([]string{"unscoped"}))
		})
		It("should check availability and NodeClaim compatibility independently", func() {
			// The only available DRA offering is in zone-b, but the set is still a device source for a zone-a NodeClaim.
			// The offering checks that follow allocation reject the instance type.
			nc.Requirements = scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))
			Expect(slicesFor(&draNodeClaim{nc: nc})).To(Equal([]string{"unscoped", "dra"}))
		})
		It("should tag each slice with its set's requirements", func() {
			slices := (&draNodeClaim{nc: nc}).ResourceSlices()[unique.Make("it-1")]
			Expect(slices[0].TemplateRequirements()).To(BeEmpty())
			Expect(slices[1].TemplateRequirements().Get(modeKey).Values()).To(ConsistOf("dra"))
		})
		It("should expose every set on a pre-initialized existing node, regardless of availability", func() {
			Expect(slicesFor(draExistingNodeForTest(false, it))).To(Equal([]string{"unscoped", "dra", "other"}))
		})
	})

	Describe("draExistingNode adapter", func() {
		var it *cloudprovider.InstanceType

		BeforeEach(func() {
			it = &cloudprovider.InstanceType{
				Name: "it-1",
				DynamicResources: []*cloudprovider.DynamicResources{{
					ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-a")}},
				}},
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
		It("should not be superposed", func() {
			Expect(draExistingNodeForTest(true, it).Superposed()).To(BeFalse())
			Expect(draExistingNodeForTest(false, it).Superposed()).To(BeFalse())
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
