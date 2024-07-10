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

package v1beta1_test

import (
	"fmt"
	"strings"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	. "sigs.k8s.io/karpenter/pkg/apis/v1beta1"
)

var _ = Describe("Webhook/Validation", func() {
	var nodePool *NodePool

	BeforeEach(func() {
		nodePool = &NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodePoolSpec{
				Template: NodeClaimTemplate{
					Spec: NodeClaimSpec{
						NodeClassRef: &NodeClassReference{
							Kind: "NodeClaim",
							Name: "default",
						},
					},
				},
			},
		}
	})
	Context("Labels", func() {
		It("should allow unrecognized labels", func() {
			nodePool.Spec.Template.Labels = map[string]string{"foo": randomdata.SillyName()}
			Expect(nodePool.RuntimeValidate()).To(Succeed())
		})
		It("should fail for the karpenter.sh/nodepool label", func() {
			nodePool.Spec.Template.Labels = map[string]string{NodePoolLabelKey: randomdata.SillyName()}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
		It("should fail for invalid label keys", func() {
			nodePool.Spec.Template.Labels = map[string]string{"spaces are not allowed": randomdata.SillyName()}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
		It("should fail for invalid label values", func() {
			nodePool.Spec.Template.Labels = map[string]string{randomdata.SillyName(): "/ is not allowed"}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
		It("should fail for restricted label domains", func() {
			for label := range RestrictedLabelDomains {
				nodePool.Spec.Template.Labels = map[string]string{label + "/unknown": randomdata.SillyName()}
				Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
			}
		})
		It("should allow labels kOps require", func() {
			nodePool.Spec.Template.Labels = map[string]string{
				"kops.k8s.io/instancegroup": "karpenter-nodes",
				"kops.k8s.io/gpu":           "1",
			}
			Expect(nodePool.RuntimeValidate()).To(Succeed())
		})
		It("should allow labels in restricted domains exceptions list", func() {
			for label := range LabelDomainExceptions {
				nodePool.Spec.Template.Labels = map[string]string{
					label: "test-value",
				}
				Expect(nodePool.RuntimeValidate()).To(Succeed())
			}
		})
		It("should allow labels prefixed with the restricted domain exceptions", func() {
			for label := range LabelDomainExceptions {
				nodePool.Spec.Template.Labels = map[string]string{
					fmt.Sprintf("%s/key", label): "test-value",
				}
				Expect(nodePool.RuntimeValidate()).To(Succeed())
			}
		})
		It("should allow subdomain labels in restricted domains exceptions list", func() {
			for label := range LabelDomainExceptions {
				nodePool.Spec.Template.Labels = map[string]string{
					fmt.Sprintf("subdomain.%s", label): "test-value",
				}
				Expect(nodePool.RuntimeValidate()).To(Succeed())
			}
		})
		It("should allow subdomain labels prefixed with the restricted domain exceptions", func() {
			for label := range LabelDomainExceptions {
				nodePool.Spec.Template.Labels = map[string]string{
					fmt.Sprintf("subdomain.%s/key", label): "test-value",
				}
				Expect(nodePool.RuntimeValidate()).To(Succeed())
			}
		})
	})
	Context("Taints", func() {
		It("should succeed for valid taints", func() {
			nodePool.Spec.Template.Spec.Taints = []v1.Taint{
				{Key: "a", Value: "b", Effect: v1.TaintEffectNoSchedule},
				{Key: "c", Value: "d", Effect: v1.TaintEffectNoExecute},
				{Key: "e", Value: "f", Effect: v1.TaintEffectPreferNoSchedule},
				{Key: "key-only", Effect: v1.TaintEffectNoExecute},
			}
			Expect(nodePool.RuntimeValidate()).To(Succeed())
		})
		It("should fail for invalid taint keys", func() {
			nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Key: "???"}}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
		It("should fail for missing taint key", func() {
			nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Effect: v1.TaintEffectNoSchedule}}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
		It("should fail for invalid taint value", func() {
			nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Key: "invalid-value", Effect: v1.TaintEffectNoSchedule, Value: "???"}}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
		It("should fail for invalid taint effect", func() {
			nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Key: "invalid-effect", Effect: "???"}}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
		It("should not fail for same key with different effects", func() {
			nodePool.Spec.Template.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoExecute},
			}
			Expect(nodePool.RuntimeValidate()).To(Succeed())
		})
		It("should fail for duplicate taint key/effect pairs", func() {
			nodePool.Spec.Template.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
			nodePool.Spec.Template.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			nodePool.Spec.Template.Spec.StartupTaints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
	})
	Context("Requirements", func() {
		It("should fail for the karpenter.sh/nodepool label", func() {
			nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: NodePoolLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{randomdata.SillyName()}}},
			}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
		It("should allow supported ops", func() {
			nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists}},
			}
			Expect(nodePool.RuntimeValidate()).To(Succeed())
		})
		It("should fail for unsupported ops", func() {
			for _, op := range []v1.NodeSelectorOperator{"unknown"} {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: op, Values: []string{"test"}}},
				}
				Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
			}
		})
		It("should fail for restricted domains", func() {
			for label := range RestrictedLabelDomains {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
			}
		})
		It("should allow restricted domains exceptions", func() {
			for label := range LabelDomainExceptions {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(nodePool.RuntimeValidate()).To(Succeed())
			}
		})
		It("should allow restricted subdomains exceptions", func() {
			for label := range LabelDomainExceptions {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "subdomain." + label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(nodePool.RuntimeValidate()).To(Succeed())
			}
		})
		It("should allow well known label exceptions", func() {
			for label := range WellKnownLabels.Difference(sets.New(NodePoolLabelKey)) {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(nodePool.RuntimeValidate()).To(Succeed())
			}
		})
		It("should allow non-empty set after removing overlapped value", func() {
			nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test", "foo"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test", "bar"}}},
			}
			Expect(nodePool.RuntimeValidate()).To(Succeed())
		})
		It("should allow empty requirements", func() {
			nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{}
			Expect(nodePool.RuntimeValidate()).To(Succeed())
		})
		It("should fail with invalid GT or LT values", func() {
			for _, requirement := range []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1", "2"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"a"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"-1"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1", "2"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"a"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"-1"}}},
			} {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{requirement}
				Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
			}
		})
		It("should error when minValues is greater than the number of unique values specified within In operator", func() {
			nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"insance-type-1"}}, MinValues: lo.ToPtr(2)},
			}
			Expect(nodePool.RuntimeValidate()).ToNot(Succeed())
		})
	})
})
