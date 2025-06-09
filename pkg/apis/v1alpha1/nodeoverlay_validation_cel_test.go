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

package v1alpha1_test

import (
	"fmt"
	"strings"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	. "sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
)

var _ = Describe("CEL/Validation", func() {
	var nodeOverlay *NodeOverlay

	BeforeEach(func() {
		if env.Version.Minor() < 25 {
			Skip("CEL Validation is for 1.25>")
		}
		nodeOverlay = &NodeOverlay{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodeOverlaySpec{
				Requirements: []corev1.NodeSelectorRequirement{
					{
						Key:      v1.CapacityTypeLabelKey,
						Operator: corev1.NodeSelectorOpExists,
					},
				},
			},
		}
	})

	It("should define either a priceAdjustment or capacity", func() {
		nodeOverlay.Spec.PriceAdjustment = ""
		nodeOverlay.Spec.Capacity = nil
		Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())

		nodeOverlay.Spec.PriceAdjustment = "1%"
		nodeOverlay.Spec.Capacity = nil
		Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())

		nodeOverlay.Spec.PriceAdjustment = ""
		nodeOverlay.Spec.Capacity = corev1.ResourceList{
			corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
		}
		Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
	})
	Context("Requirements", func() {
		It("should succeed for valid requirement keys", func() {
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
				{Key: "Test", Operator: corev1.NodeSelectorOpExists},
				{Key: "test.com/Test", Operator: corev1.NodeSelectorOpExists},
				{Key: "test.com.com/test", Operator: corev1.NodeSelectorOpExists},
				{Key: "key-only", Operator: corev1.NodeSelectorOpExists},
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).To(Succeed())
		})
		It("should fail for invalid requirement keys", func() {
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{{Key: "test.com.com}", Operator: corev1.NodeSelectorOpExists}}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{{Key: "Test.com/test", Operator: corev1.NodeSelectorOpExists}}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{{Key: "test/test/test", Operator: corev1.NodeSelectorOpExists}}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{{Key: "test/", Operator: corev1.NodeSelectorOpExists}}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{{Key: "/test", Operator: corev1.NodeSelectorOpExists}}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
		})
		It("should allow for the karpenter.sh/nodepool label", func() {
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
				{Key: v1.NodePoolLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{randomdata.SillyName()}},
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
		})
		It("should fail at runtime for requirement keys that are too long", func() {
			oldnodeOverlay := nodeOverlay.DeepCopy()
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{{Key: fmt.Sprintf("test.com.test.%s/test", strings.ToLower(randomdata.Alphanumeric(250))), Operator: corev1.NodeSelectorOpExists}}
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
			Expect(env.Client.Delete(ctx, nodeOverlay)).To(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
			nodeOverlay = oldnodeOverlay.DeepCopy()
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{{Key: fmt.Sprintf("test.com.test/test-%s", strings.ToLower(randomdata.Alphanumeric(250))), Operator: corev1.NodeSelectorOpExists}}
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
		})
		It("should allow supported ops", func() {
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpGt, Values: []string{"1"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpLt, Values: []string{"1"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpNotIn},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpExists},
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).To(Succeed())
		})
		It("should fail for unsupported ops", func() {
			for _, op := range []corev1.NodeSelectorOperator{"unknown"} {
				nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
					{Key: corev1.LabelTopologyZone, Operator: op, Values: []string{"test"}},
				}
				Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
				Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
			}
		})
		It("should fail for restricted domains", func() {
			for label := range v1.RestrictedLabelDomains {
				nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
					{Key: label + "/test", Operator: corev1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
				Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
			}
		})
		It("should allow restricted domains exceptions", func() {
			oldnodeOverlay := nodeOverlay.DeepCopy()
			for label := range v1.LabelDomainExceptions {
				nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
					{Key: label + "/test", Operator: corev1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
				Expect(nodeOverlay.RuntimeValidate(ctx)).To(Succeed())
				Expect(env.Client.Delete(ctx, nodeOverlay)).To(Succeed())
				nodeOverlay = oldnodeOverlay.DeepCopy()
			}
		})
		It("should allow restricted subdomains exceptions", func() {
			oldnodeOverlay := nodeOverlay.DeepCopy()
			for label := range v1.LabelDomainExceptions {
				nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
					{Key: "subdomain." + label + "/test", Operator: corev1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
				Expect(nodeOverlay.RuntimeValidate(ctx)).To(Succeed())
				Expect(env.Client.Delete(ctx, nodeOverlay)).To(Succeed())
				nodeOverlay = oldnodeOverlay.DeepCopy()
			}
		})
		It("should allow well known label exceptions", func() {
			oldnodeOverlay := nodeOverlay.DeepCopy()
			// Capacity Type is runtime validated
			for label := range v1.WellKnownLabels.Difference(sets.New(v1.NodePoolLabelKey, v1.CapacityTypeLabelKey)) {
				nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
					{Key: label, Operator: corev1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
				Expect(nodeOverlay.RuntimeValidate(ctx)).To(Succeed())
				Expect(env.Client.Delete(ctx, nodeOverlay)).To(Succeed())
				nodeOverlay = oldnodeOverlay.DeepCopy()
			}
		})
		It("should allow non-empty set after removing overlapped value", func() {
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test", "foo"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"test", "bar"}},
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).To(Succeed())
		})
		It("should allow empty requirements", func() {
			nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{}
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
			Expect(nodeOverlay.RuntimeValidate(ctx)).To(Succeed())
		})
		It("should fail with invalid GT or LT values", func() {
			for _, requirement := range []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpGt, Values: []string{}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpGt, Values: []string{"1", "2"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpGt, Values: []string{"a"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpGt, Values: []string{"-1"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpLt, Values: []string{}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpLt, Values: []string{"1", "2"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpLt, Values: []string{"a"}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpLt, Values: []string{"-1"}},
			} {
				nodeOverlay.Spec.Requirements = []corev1.NodeSelectorRequirement{requirement}
				Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
				Expect(nodeOverlay.RuntimeValidate(ctx)).ToNot(Succeed())
			}
		})
	})
	Context("priceAdjustment", func() {
		It("should allow percentage value for priceAdjustment field", func() {
			nodeOverlay.Spec.PriceAdjustment = "1%"
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
		})
		It("should allow percentage greater then 100%", func() {
			nodeOverlay.Spec.PriceAdjustment = "123%"
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
		})
		It("should allow for integer value for ", func() {
			nodeOverlay.Spec.PriceAdjustment = "43"
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
		})
		It("should only allow whole number percentage value ", func() {
			nodeOverlay.Spec.PriceAdjustment = "343.5%"
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
		})
		It("should limit percentage value to be no less then 1%", func() {
			nodeOverlay.Spec.PriceAdjustment = "0.5%"
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
		})
		It("should allow percentage greater then 100%", func() {
			nodeOverlay.Spec.PriceAdjustment = "123%"
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
		})
		It("should not allow for float value", func() {
			nodeOverlay.Spec.PriceAdjustment = "34.43"
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
		})
	})
	Context("Capacity", func() {
		It("should allow custom resources", func() {
			nodeOverlay.Spec.Capacity = corev1.ResourceList{
				corev1.ResourceName("smarter-devices/fuse"): resource.MustParse("1"),
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).To(Succeed())
		})
		It("should not allow cpu resources override", func() {
			nodeOverlay.Spec.Capacity = corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
		})
		It("should not allow memory resources override", func() {
			nodeOverlay.Spec.Capacity = corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("34Gi"),
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
		})
		It("should not allow ephemeral-storage resources override", func() {
			nodeOverlay.Spec.Capacity = corev1.ResourceList{
				corev1.ResourceEphemeralStorage: resource.MustParse("34Gi"),
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
		})
		It("should not allow pod resources override", func() {
			nodeOverlay.Spec.Capacity = corev1.ResourceList{
				corev1.ResourcePods: resource.MustParse("324"),
			}
			Expect(env.Client.Create(ctx, nodeOverlay)).ToNot(Succeed())
		})
	})
})
