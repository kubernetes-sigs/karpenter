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

package v1_test

import (
	"strings"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	. "sigs.k8s.io/karpenter/pkg/apis/v1"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

var _ = Describe("Validation", func() {
	var nodeClaim *NodeClaim

	BeforeEach(func() {
		nodeClaim = &NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
		}
	})

	Context("Taints", func() {
		It("should succeed for valid taints", func() {
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Value: "b", Effect: v1.TaintEffectNoSchedule},
				{Key: "c", Value: "d", Effect: v1.TaintEffectNoExecute},
				{Key: "e", Value: "f", Effect: v1.TaintEffectPreferNoSchedule},
				{Key: "key-only", Effect: v1.TaintEffectNoExecute},
			}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
		})
		It("should fail for invalid taint keys", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "???"}}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for missing taint key", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Effect: v1.TaintEffectNoSchedule}}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for invalid taint value", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "invalid-value", Effect: v1.TaintEffectNoSchedule, Value: "???"}}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for invalid taint effect", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "invalid-effect", Effect: "???"}}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should not fail for same key with different effects", func() {
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoExecute},
			}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
		})
		It("should fail for duplicate taint key/effect pairs", func() {
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			nodeClaim.Spec.StartupTaints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
	})
	Context("Requirements", func() {
		It("should allow supported ops", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists}},
			}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
		})
		It("should fail for unsupported ops", func() {
			for _, op := range []v1.NodeSelectorOperator{"unknown"} {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: op, Values: []string{"test"}}},
				}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			}
		})
		It("should fail for restricted domains", func() {
			for label := range RestrictedLabelDomains {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			}
		})
		It("should allow restricted domains exceptions", func() {
			for label := range LabelDomainExceptions {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(nodeClaim.Validate(ctx)).To(Succeed())
			}
		})
		It("should allow restricted subdomains exceptions", func() {
			for label := range LabelDomainExceptions {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "subdomain." + label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(nodeClaim.Validate(ctx)).To(Succeed())
			}
		})
		It("should allow well known label exceptions", func() {
			for label := range WellKnownLabels.Difference(sets.New(NodePoolLabelKey)) {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(nodeClaim.Validate(ctx)).To(Succeed())
			}
		})
		It("should allow non-empty set after removing overlapped value", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test", "foo"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test", "bar"}}},
			}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
		})
		It("should allow empty requirements", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
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
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{requirement}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			}
		})
		It("should error when minValues is greater than the number of unique values specified within In operator", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"insance-type-1"}}, MinValues: lo.ToPtr(2)},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
	})
})
