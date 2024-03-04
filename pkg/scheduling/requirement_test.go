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
	"math"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
)

var _ = Describe("Requirement", func() {
	// Requirements created without minValues
	exists := NewRequirement("key", v1.NodeSelectorOpExists)
	doesNotExist := NewRequirement("key", v1.NodeSelectorOpDoesNotExist)
	inA := NewRequirement("key", v1.NodeSelectorOpIn, "A")
	inB := NewRequirement("key", v1.NodeSelectorOpIn, "B")
	inAB := NewRequirement("key", v1.NodeSelectorOpIn, "A", "B")
	notInA := NewRequirement("key", v1.NodeSelectorOpNotIn, "A")
	in1 := NewRequirement("key", v1.NodeSelectorOpIn, "1")
	in9 := NewRequirement("key", v1.NodeSelectorOpIn, "9")
	in19 := NewRequirement("key", v1.NodeSelectorOpIn, "1", "9")
	notIn12 := NewRequirement("key", v1.NodeSelectorOpNotIn, "1", "2")
	greaterThan1 := NewRequirement("key", v1.NodeSelectorOpGt, "1")
	greaterThan9 := NewRequirement("key", v1.NodeSelectorOpGt, "9")
	lessThan1 := NewRequirement("key", v1.NodeSelectorOpLt, "1")
	lessThan9 := NewRequirement("key", v1.NodeSelectorOpLt, "9")

	// Requirements created with minValues flexibility
	existsOperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpExists, lo.ToPtr(1))
	doesNotExistOperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpDoesNotExist, lo.ToPtr(1))
	inAOperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpIn, lo.ToPtr(1), "A")
	inBOperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpIn, lo.ToPtr(1), "B")
	inABOperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpIn, lo.ToPtr(2), "A", "B")
	notInAOperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpNotIn, lo.ToPtr(1), "A")
	in1OperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpIn, lo.ToPtr(1), "1")
	in9OperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpIn, lo.ToPtr(1), "9")
	in19OperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpIn, lo.ToPtr(2), "1", "9")
	notIn12OperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpNotIn, lo.ToPtr(2), "1", "2")
	greaterThan1OperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpGt, lo.ToPtr(1), "1")
	greaterThan9OperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpGt, lo.ToPtr(1), "9")
	lessThan1OperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpLt, lo.ToPtr(1), "1")
	lessThan9OperatorWithFlexibility := NewRequirementWithFlexibility("key", v1.NodeSelectorOpLt, lo.ToPtr(1), "9")

	Context("NewRequirements", func() {
		It("should normalize labels", func() {
			nodeSelector := map[string]string{
				v1.LabelFailureDomainBetaZone:   "test",
				v1.LabelFailureDomainBetaRegion: "test",
				"beta.kubernetes.io/arch":       "test",
				"beta.kubernetes.io/os":         "test",
				v1.LabelInstanceType:            "test",
			}
			requirements := lo.MapToSlice(nodeSelector, func(key string, value string) v1.NodeSelectorRequirement {
				return v1.NodeSelectorRequirement{Key: key, Operator: v1.NodeSelectorOpIn, Values: []string{value}}
			})
			for _, r := range []Requirements{
				NewLabelRequirements(nodeSelector),
				NewNodeSelectorRequirements(requirements...),
				NewPodRequirements(&v1.Pod{
					Spec: v1.PodSpec{
						NodeSelector: nodeSelector,
						Affinity: &v1.Affinity{
							NodeAffinity: &v1.NodeAffinity{
								RequiredDuringSchedulingIgnoredDuringExecution:  &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{{MatchExpressions: requirements}}},
								PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{{Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: requirements}}},
							},
						},
					},
				}),
			} {
				Expect(sets.List(r.Keys())).To(ConsistOf(
					v1.LabelArchStable,
					v1.LabelOSStable,
					v1.LabelInstanceTypeStable,
					v1.LabelTopologyRegion,
					v1.LabelTopologyZone,
				))
			}
		})
	})
	Context("Intersect requirements", func() {
		DescribeTable("should intersect sets for existing requirement without minValues and new requirement without minValues",
			func(existingRequirementWithoutMinValues, newRequirementWithoutMinValues, expectedRequirement *Requirement) {
				Expect(existingRequirementWithoutMinValues.Intersection(newRequirementWithoutMinValues)).To(Equal(expectedRequirement))
			},
			Entry(exists, exists, exists),
			Entry(exists, doesNotExist, doesNotExist),
			Entry(exists, inA, inA),
			Entry(exists, inB, inB),
			Entry(exists, inAB, inAB),
			Entry(exists, notInA, notInA),
			Entry(exists, in1, in1),
			Entry(exists, in9, in9),
			Entry(exists, in19, in19),
			Entry(exists, notIn12, notIn12),
			Entry(exists, greaterThan1, greaterThan1),
			Entry(exists, greaterThan9, greaterThan9),
			Entry(exists, lessThan1, lessThan1),
			Entry(exists, lessThan9, lessThan9),

			Entry(doesNotExist, exists, doesNotExist),
			Entry(doesNotExist, doesNotExist, doesNotExist),
			Entry(doesNotExist, inA, doesNotExist),
			Entry(doesNotExist, inB, doesNotExist),
			Entry(doesNotExist, inAB, doesNotExist),
			Entry(doesNotExist, notInA, doesNotExist),
			Entry(doesNotExist, in1, doesNotExist),
			Entry(doesNotExist, in9, doesNotExist),
			Entry(doesNotExist, in19, doesNotExist),
			Entry(doesNotExist, notIn12, doesNotExist),
			Entry(doesNotExist, greaterThan1, doesNotExist),
			Entry(doesNotExist, greaterThan9, doesNotExist),
			Entry(doesNotExist, lessThan1, doesNotExist),
			Entry(doesNotExist, lessThan9, doesNotExist),

			Entry(inA, exists, inA),
			Entry(inA, doesNotExist, doesNotExist),
			Entry(inA, inA, inA),
			Entry(inA, inB, doesNotExist),
			Entry(inA, inAB, inA),
			Entry(inA, notInA, doesNotExist),
			Entry(inA, in1, doesNotExist),
			Entry(inA, in9, doesNotExist),
			Entry(inA, in19, doesNotExist),
			Entry(inA, notIn12, inA),
			Entry(inA, greaterThan1, doesNotExist),
			Entry(inA, greaterThan9, doesNotExist),
			Entry(inA, lessThan1, doesNotExist),
			Entry(inA, lessThan9, doesNotExist),

			Entry(inB, exists, inB),
			Entry(inB, doesNotExist, doesNotExist),
			Entry(inB, inA, doesNotExist),
			Entry(inB, inB, inB),
			Entry(inB, inAB, inB),
			Entry(inB, notInA, inB),
			Entry(inB, in1, doesNotExist),
			Entry(inB, in9, doesNotExist),
			Entry(inB, in19, doesNotExist),
			Entry(inB, notIn12, inB),
			Entry(inB, greaterThan1, doesNotExist),
			Entry(inB, greaterThan9, doesNotExist),
			Entry(inB, lessThan1, doesNotExist),
			Entry(inB, lessThan9, doesNotExist),

			Entry(inAB, exists, inAB),
			Entry(inAB, doesNotExist, doesNotExist),
			Entry(inAB, inA, inA),
			Entry(inAB, inB, inB),
			Entry(inAB, inAB, inAB),
			Entry(inAB, notInA, inB),
			Entry(inAB, in1, doesNotExist),
			Entry(inAB, in9, doesNotExist),
			Entry(inAB, in19, doesNotExist),
			Entry(inAB, notIn12, inAB),
			Entry(inAB, greaterThan1, doesNotExist),
			Entry(inAB, greaterThan9, doesNotExist),
			Entry(inAB, lessThan1, doesNotExist),
			Entry(inAB, lessThan9, doesNotExist),

			Entry(notInA, exists, notInA),
			Entry(notInA, doesNotExist, doesNotExist),
			Entry(notInA, inA, doesNotExist),
			Entry(notInA, inB, inB),
			Entry(notInA, inAB, inB),
			Entry(notInA, notInA, notInA),
			Entry(notInA, in1, in1),
			Entry(notInA, in9, in9),
			Entry(notInA, in19, in19),
			Entry(notInA, notIn12, &Requirement{Key: "key", complement: true, values: sets.New("A", "1", "2")}),
			Entry(notInA, greaterThan1, greaterThan1),
			Entry(notInA, greaterThan9, greaterThan9),
			Entry(notInA, lessThan1, lessThan1),
			Entry(notInA, lessThan9, lessThan9),

			Entry(in1, exists, in1),
			Entry(in1, doesNotExist, doesNotExist),
			Entry(in1, inA, doesNotExist),
			Entry(in1, inB, doesNotExist),
			Entry(in1, inAB, doesNotExist),
			Entry(in1, notInA, in1),
			Entry(in1, in1, in1),
			Entry(in1, in9, doesNotExist),
			Entry(in1, in19, in1),
			Entry(in1, notIn12, doesNotExist),
			Entry(in1, greaterThan1, doesNotExist),
			Entry(in1, greaterThan9, doesNotExist),
			Entry(in1, lessThan1, doesNotExist),
			Entry(in1, lessThan9, in1),

			Entry(in9, exists, in9),
			Entry(in9, doesNotExist, doesNotExist),
			Entry(in9, inA, doesNotExist),
			Entry(in9, inB, doesNotExist),
			Entry(in9, inAB, doesNotExist),
			Entry(in9, notInA, in9),
			Entry(in9, in1, doesNotExist),
			Entry(in9, in9, in9),
			Entry(in9, in19, in9),
			Entry(in9, notIn12, in9),
			Entry(in9, greaterThan1, in9),
			Entry(in9, greaterThan9, doesNotExist),
			Entry(in9, lessThan1, doesNotExist),
			Entry(in9, lessThan9, doesNotExist),

			Entry(in19, exists, in19),
			Entry(in19, doesNotExist, doesNotExist),
			Entry(in19, inA, doesNotExist),
			Entry(in19, inB, doesNotExist),
			Entry(in19, inAB, doesNotExist),
			Entry(in19, notInA, in19),
			Entry(in19, in1, in1),
			Entry(in19, in9, in9),
			Entry(in19, in19, in19),
			Entry(in19, notIn12, in9),
			Entry(in19, greaterThan1, in9),
			Entry(in19, greaterThan9, doesNotExist),
			Entry(in19, lessThan1, doesNotExist),
			Entry(in19, lessThan9, in1),

			Entry(notIn12, exists, notIn12),
			Entry(notIn12, doesNotExist, doesNotExist),
			Entry(notIn12, inA, inA),
			Entry(notIn12, inB, inB),
			Entry(notIn12, inAB, inAB),
			Entry(notIn12, notInA, &Requirement{Key: "key", complement: true, values: sets.New("A", "1", "2")}),
			Entry(notIn12, in1, doesNotExist),
			Entry(notIn12, in9, in9),
			Entry(notIn12, in19, in9),
			Entry(notIn12, notIn12, notIn12),
			Entry(notIn12, greaterThan1, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, values: sets.New("2")}),
			Entry(notIn12, greaterThan9, &Requirement{Key: "key", complement: true, greaterThan: greaterThan9.greaterThan, values: sets.New[string]()}),
			Entry(notIn12, lessThan1, &Requirement{Key: "key", complement: true, lessThan: lessThan1.lessThan, values: sets.New[string]()}),
			Entry(notIn12, lessThan9, &Requirement{Key: "key", complement: true, lessThan: lessThan9.lessThan, values: sets.New("1", "2")}),

			Entry(greaterThan1, exists, greaterThan1),
			Entry(greaterThan1, doesNotExist, doesNotExist),
			Entry(greaterThan1, inA, doesNotExist),
			Entry(greaterThan1, inB, doesNotExist),
			Entry(greaterThan1, inAB, doesNotExist),
			Entry(greaterThan1, notInA, greaterThan1),
			Entry(greaterThan1, in1, doesNotExist),
			Entry(greaterThan1, in9, in9),
			Entry(greaterThan1, in19, in9),
			Entry(greaterThan1, notIn12, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, values: sets.New("2")}),
			Entry(greaterThan1, greaterThan1, greaterThan1),
			Entry(greaterThan1, greaterThan9, greaterThan9),
			Entry(greaterThan1, lessThan1, doesNotExist),
			Entry(greaterThan1, lessThan9, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, lessThan: lessThan9.lessThan, values: sets.New[string]()}),

			Entry(greaterThan9, exists, greaterThan9),
			Entry(greaterThan9, doesNotExist, doesNotExist),
			Entry(greaterThan9, inA, doesNotExist),
			Entry(greaterThan9, inB, doesNotExist),
			Entry(greaterThan9, inAB, doesNotExist),
			Entry(greaterThan9, notInA, greaterThan9),
			Entry(greaterThan9, in1, doesNotExist),
			Entry(greaterThan9, in9, doesNotExist),
			Entry(greaterThan9, in19, doesNotExist),
			Entry(greaterThan9, notIn12, greaterThan9),
			Entry(greaterThan9, greaterThan1, greaterThan9),
			Entry(greaterThan9, greaterThan9, greaterThan9),
			Entry(greaterThan9, lessThan1, doesNotExist),
			Entry(greaterThan9, lessThan9, doesNotExist),

			Entry(lessThan1, exists, lessThan1),
			Entry(lessThan1, doesNotExist, doesNotExist),
			Entry(lessThan1, inA, doesNotExist),
			Entry(lessThan1, inB, doesNotExist),
			Entry(lessThan1, inAB, doesNotExist),
			Entry(lessThan1, notInA, lessThan1),
			Entry(lessThan1, in1, doesNotExist),
			Entry(lessThan1, in9, doesNotExist),
			Entry(lessThan1, in19, doesNotExist),
			Entry(lessThan1, notIn12, lessThan1),
			Entry(lessThan1, greaterThan1, doesNotExist),
			Entry(lessThan1, greaterThan9, doesNotExist),
			Entry(lessThan1, lessThan1, lessThan1),
			Entry(lessThan1, lessThan9, lessThan1),

			Entry(lessThan9, exists, lessThan9),
			Entry(lessThan9, doesNotExist, doesNotExist),
			Entry(lessThan9, inA, doesNotExist),
			Entry(lessThan9, inB, doesNotExist),
			Entry(lessThan9, inAB, doesNotExist),
			Entry(lessThan9, notInA, lessThan9),
			Entry(lessThan9, in1, in1),
			Entry(lessThan9, in9, doesNotExist),
			Entry(lessThan9, in19, in1),
			Entry(lessThan9, notIn12, &Requirement{Key: "key", complement: true, lessThan: lessThan9.lessThan, values: sets.New("1", "2")}),
			Entry(lessThan9, greaterThan1, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, lessThan: lessThan9.lessThan, values: sets.New[string]()}),
			Entry(lessThan9, greaterThan9, doesNotExist),
			Entry(lessThan9, lessThan1, lessThan1),
			Entry(lessThan9, lessThan9, lessThan9),
		)
		DescribeTable("should intersect sets for existing requirement with minValues and new requirement without minValues",
			func(existingRequirementWithMinValues, newRequirementWithoutMinValues, expectedRequirement *Requirement) {
				Expect(existingRequirementWithMinValues.Intersection(newRequirementWithoutMinValues)).To(Equal(expectedRequirement))
			},
			Entry(existsOperatorWithFlexibility, exists, existsOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, inA, inAOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, inB, inBOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, inAB, &Requirement{Key: "key", complement: false, values: sets.New("A", "B"), MinValues: lo.ToPtr(1)}),
			Entry(existsOperatorWithFlexibility, notInA, notInAOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, in1, in1OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, in9, in9OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, in19, &Requirement{Key: "key", complement: false, values: sets.New("1", "9"), MinValues: lo.ToPtr(1)}),
			Entry(existsOperatorWithFlexibility, notIn12, &Requirement{Key: "key", complement: true, values: sets.New("1", "2"), MinValues: lo.ToPtr(1)}),
			Entry(existsOperatorWithFlexibility, greaterThan1, greaterThan1OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, greaterThan9, greaterThan9OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, lessThan1, lessThan1OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, lessThan9, lessThan9OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, exists, existsOperatorWithFlexibility),

			Entry(doesNotExistOperatorWithFlexibility, exists, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, inB, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, inAB, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, notInA, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, in1, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, in9, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, in19, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, notIn12, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, greaterThan1, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, greaterThan9, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, lessThan1, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, lessThan9, doesNotExistOperatorWithFlexibility),

			Entry(inAOperatorWithFlexibility, exists, inAOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, inA, inAOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, inB, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, inAB, inAOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, notInA, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, in1, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, in9, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, in19, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, notIn12, inAOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, greaterThan1, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, greaterThan9, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, lessThan1, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, lessThan9, doesNotExistOperatorWithFlexibility),

			Entry(inBOperatorWithFlexibility, exists, inBOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, inB, inBOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, inAB, inBOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, notInA, inBOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, in1, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, in9, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, in19, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, notIn12, inBOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, greaterThan1, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, greaterThan9, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, lessThan1, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, lessThan9, doesNotExistOperatorWithFlexibility),

			Entry(inABOperatorWithFlexibility, exists, inABOperatorWithFlexibility),
			Entry(inABOperatorWithFlexibility, doesNotExist, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, inA, &Requirement{Key: "key", complement: false, values: sets.New("A"), MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, inB, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, inAB, inABOperatorWithFlexibility),
			Entry(inABOperatorWithFlexibility, notInA, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, in1, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, in9, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, in19, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, notIn12, &Requirement{Key: "key", complement: false, values: sets.New("A", "B"), MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, greaterThan1, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, greaterThan9, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, lessThan1, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, lessThan9, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),

			Entry(notInAOperatorWithFlexibility, exists, notInAOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, inB, inBOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, inAB, inBOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, notInA, notInAOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, in1, in1OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, in9, in9OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, in19, &Requirement{Key: "key", complement: false, values: sets.New("1", "9"), MinValues: lo.ToPtr(1)}),
			Entry(notInAOperatorWithFlexibility, notIn12, &Requirement{Key: "key", complement: true, values: sets.New("A", "1", "2"), MinValues: lo.ToPtr(1)}),
			Entry(notInAOperatorWithFlexibility, greaterThan1, greaterThan1OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, greaterThan9, greaterThan9OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, lessThan1, lessThan1OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, lessThan9, lessThan9OperatorWithFlexibility),

			Entry(in1OperatorWithFlexibility, exists, in1OperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, inB, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, inAB, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, notInA, in1OperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, in1, in1OperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, in9, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, in19, in1OperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, notIn12, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, greaterThan1, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, greaterThan9, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, lessThan1, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, lessThan9, in1OperatorWithFlexibility),

			Entry(in9OperatorWithFlexibility, exists, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, inB, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, inAB, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, notInA, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, in1, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, in9, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, in19, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, notIn12, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, greaterThan1, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, greaterThan9, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, lessThan1, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, lessThan9, doesNotExistOperatorWithFlexibility),

			Entry(in19OperatorWithFlexibility, exists, in19OperatorWithFlexibility),
			Entry(in19OperatorWithFlexibility, doesNotExist, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, inA, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, inB, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, inAB, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, notInA, &Requirement{Key: "key", complement: false, values: sets.New("1", "9"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, in1, &Requirement{Key: "key", complement: false, values: sets.New("1"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, in9, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, in19, in19OperatorWithFlexibility),
			Entry(in19OperatorWithFlexibility, notIn12, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, greaterThan1, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, greaterThan9, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, lessThan1, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, lessThan9, &Requirement{Key: "key", complement: false, values: sets.New("1"), MinValues: lo.ToPtr(2)}),

			Entry(notIn12OperatorWithFlexibility, exists, notIn12OperatorWithFlexibility),
			Entry(notIn12OperatorWithFlexibility, doesNotExist, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, inA, &Requirement{Key: "key", complement: false, values: sets.New("A"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, inB, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, inAB, inABOperatorWithFlexibility),
			Entry(notIn12OperatorWithFlexibility, notInA, &Requirement{Key: "key", complement: true, values: sets.New("A", "1", "2"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, in1, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, in9, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, in19, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, notIn12, notIn12OperatorWithFlexibility),
			Entry(notIn12OperatorWithFlexibility, greaterThan1, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, values: sets.New("2"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, greaterThan9, &Requirement{Key: "key", complement: true, greaterThan: greaterThan9.greaterThan, values: sets.New[string](), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, lessThan1, &Requirement{Key: "key", complement: true, lessThan: lessThan1.lessThan, values: sets.New[string](), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, lessThan9, &Requirement{Key: "key", complement: true, lessThan: lessThan9.lessThan, values: sets.New("1", "2"), MinValues: lo.ToPtr(2)}),

			Entry(greaterThan1OperatorWithFlexibility, exists, greaterThan1OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, inB, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, inAB, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, notInA, greaterThan1OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, in1, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, in9, in9OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, in19, in9OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, notIn12, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, values: sets.New("2"), MinValues: lo.ToPtr(1)}),
			Entry(greaterThan1OperatorWithFlexibility, greaterThan1, greaterThan1OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, greaterThan9, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, lessThan1, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, lessThan9, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, lessThan: lessThan9.lessThan, values: sets.New[string](), MinValues: lo.ToPtr(1)}),

			Entry(greaterThan9OperatorWithFlexibility, exists, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, inB, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, inAB, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, notInA, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, in1, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, in9, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, in19, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, notIn12, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, greaterThan1, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, greaterThan9, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, lessThan1, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, lessThan9, doesNotExistOperatorWithFlexibility),

			Entry(lessThan1OperatorWithFlexibility, exists, lessThan1OperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, inB, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, inAB, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, notInA, lessThan1OperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, in1, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, in9, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, in19, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, notIn12, lessThan1OperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, greaterThan1, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, greaterThan9, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, lessThan1, lessThan1OperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, lessThan9, lessThan1OperatorWithFlexibility),

			Entry(lessThan9OperatorWithFlexibility, exists, lessThan9OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, doesNotExist, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, inA, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, inB, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, inAB, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, notInA, lessThan9OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, in1, in1OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, in9, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, in19, in1OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, notIn12, &Requirement{Key: "key", complement: true, lessThan: lessThan9.lessThan, values: sets.New("1", "2"), MinValues: lo.ToPtr(1)}),
			Entry(lessThan9OperatorWithFlexibility, greaterThan1, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, lessThan: lessThan9.lessThan, values: sets.New[string](), MinValues: lo.ToPtr(1)}),
			Entry(lessThan9OperatorWithFlexibility, greaterThan9, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, lessThan1, lessThan1OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, lessThan9, lessThan9OperatorWithFlexibility),
		)
		DescribeTable("should intersect sets for existing requirement with minValues and new requirement with minValues",
			func(existingRequirementWithMinValues, newRequirementWithMinValues, expectedRequirement *Requirement) {
				Expect(existingRequirementWithMinValues.Intersection(newRequirementWithMinValues)).To(Equal(expectedRequirement))
			},
			Entry(existsOperatorWithFlexibility, existsOperatorWithFlexibility, existsOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, inAOperatorWithFlexibility, inAOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, inBOperatorWithFlexibility, inBOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("A", "B"), MinValues: lo.ToPtr(2)}),
			Entry(existsOperatorWithFlexibility, notInAOperatorWithFlexibility, notInAOperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, in1OperatorWithFlexibility, in1OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, in9OperatorWithFlexibility, in9OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("1", "9"), MinValues: lo.ToPtr(2)}),
			Entry(existsOperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: true, values: sets.New("1", "2"), MinValues: lo.ToPtr(2)}),
			Entry(existsOperatorWithFlexibility, greaterThan1OperatorWithFlexibility, greaterThan1OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, greaterThan9OperatorWithFlexibility, greaterThan9OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, lessThan1OperatorWithFlexibility, lessThan1OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, lessThan9OperatorWithFlexibility, lessThan9OperatorWithFlexibility),
			Entry(existsOperatorWithFlexibility, existsOperatorWithFlexibility, existsOperatorWithFlexibility),

			Entry(doesNotExistOperatorWithFlexibility, existsOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(doesNotExistOperatorWithFlexibility, notInAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, in1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, in9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(doesNotExistOperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(doesNotExistOperatorWithFlexibility, greaterThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, greaterThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, lessThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(doesNotExistOperatorWithFlexibility, lessThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),

			Entry(inAOperatorWithFlexibility, existsOperatorWithFlexibility, inAOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, inAOperatorWithFlexibility, inAOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("A"), MinValues: lo.ToPtr(2)}),
			Entry(inAOperatorWithFlexibility, notInAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, in1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, in9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inAOperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("A"), MinValues: lo.ToPtr(2)}),
			Entry(inAOperatorWithFlexibility, greaterThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, greaterThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, lessThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inAOperatorWithFlexibility, lessThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),

			Entry(inBOperatorWithFlexibility, existsOperatorWithFlexibility, inBOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, inBOperatorWithFlexibility, inBOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(inBOperatorWithFlexibility, notInAOperatorWithFlexibility, inBOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, in1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, in9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inBOperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(inBOperatorWithFlexibility, greaterThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, greaterThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, lessThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(inBOperatorWithFlexibility, lessThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),

			Entry(inABOperatorWithFlexibility, existsOperatorWithFlexibility, inABOperatorWithFlexibility),
			Entry(inABOperatorWithFlexibility, doesNotExistOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, inAOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("A"), MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, inBOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, inABOperatorWithFlexibility, inABOperatorWithFlexibility),
			Entry(inABOperatorWithFlexibility, notInAOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, in1OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, in9OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("A", "B"), MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, greaterThan1OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, greaterThan9OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, lessThan1OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(inABOperatorWithFlexibility, lessThan9OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),

			Entry(notInAOperatorWithFlexibility, existsOperatorWithFlexibility, notInAOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, inBOperatorWithFlexibility, inBOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(notInAOperatorWithFlexibility, notInAOperatorWithFlexibility, notInAOperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, in1OperatorWithFlexibility, in1OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, in9OperatorWithFlexibility, in9OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("1", "9"), MinValues: lo.ToPtr(2)}),
			Entry(notInAOperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: true, values: sets.New("A", "1", "2"), MinValues: lo.ToPtr(2)}),
			Entry(notInAOperatorWithFlexibility, greaterThan1OperatorWithFlexibility, greaterThan1OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, greaterThan9OperatorWithFlexibility, greaterThan9OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, lessThan1OperatorWithFlexibility, lessThan1OperatorWithFlexibility),
			Entry(notInAOperatorWithFlexibility, lessThan9OperatorWithFlexibility, lessThan9OperatorWithFlexibility),

			Entry(in1OperatorWithFlexibility, existsOperatorWithFlexibility, in1OperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in1OperatorWithFlexibility, notInAOperatorWithFlexibility, in1OperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, in1OperatorWithFlexibility, in1OperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, in9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("1"), MinValues: lo.ToPtr(2)}),
			Entry(in1OperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in1OperatorWithFlexibility, greaterThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, greaterThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, lessThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in1OperatorWithFlexibility, lessThan9OperatorWithFlexibility, in1OperatorWithFlexibility),

			Entry(in9OperatorWithFlexibility, existsOperatorWithFlexibility, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in9OperatorWithFlexibility, notInAOperatorWithFlexibility, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, in1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, in9OperatorWithFlexibility, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(in9OperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(in9OperatorWithFlexibility, greaterThan1OperatorWithFlexibility, in9OperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, greaterThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, lessThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(in9OperatorWithFlexibility, lessThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),

			Entry(in19OperatorWithFlexibility, existsOperatorWithFlexibility, in19OperatorWithFlexibility),
			Entry(in19OperatorWithFlexibility, doesNotExistOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, inAOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, inBOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, notInAOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("1", "9"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, in1OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("1"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, in9OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, in19OperatorWithFlexibility, in19OperatorWithFlexibility),
			Entry(in19OperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, greaterThan1OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, greaterThan9OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, lessThan1OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(in19OperatorWithFlexibility, lessThan9OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("1"), MinValues: lo.ToPtr(2)}),

			Entry(notIn12OperatorWithFlexibility, existsOperatorWithFlexibility, notIn12OperatorWithFlexibility),
			Entry(notIn12OperatorWithFlexibility, doesNotExistOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, inAOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("A"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, inBOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("B"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, inABOperatorWithFlexibility, inABOperatorWithFlexibility),
			Entry(notIn12OperatorWithFlexibility, notInAOperatorWithFlexibility, &Requirement{Key: "key", complement: true, values: sets.New("A", "1", "2"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, in1OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, in9OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, notIn12OperatorWithFlexibility, notIn12OperatorWithFlexibility),
			Entry(notIn12OperatorWithFlexibility, greaterThan1OperatorWithFlexibility, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, values: sets.New("2"), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, greaterThan9OperatorWithFlexibility, &Requirement{Key: "key", complement: true, greaterThan: greaterThan9.greaterThan, values: sets.New[string](), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, lessThan1OperatorWithFlexibility, &Requirement{Key: "key", complement: true, lessThan: lessThan1.lessThan, values: sets.New[string](), MinValues: lo.ToPtr(2)}),
			Entry(notIn12OperatorWithFlexibility, lessThan9OperatorWithFlexibility, &Requirement{Key: "key", complement: true, lessThan: lessThan9.lessThan, values: sets.New("1", "2"), MinValues: lo.ToPtr(2)}),

			Entry(greaterThan1OperatorWithFlexibility, existsOperatorWithFlexibility, greaterThan1OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(greaterThan1OperatorWithFlexibility, notInAOperatorWithFlexibility, greaterThan1OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, in1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, in9OperatorWithFlexibility, in9OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("9"), MinValues: lo.ToPtr(2)}),
			Entry(greaterThan1OperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, values: sets.New("2"), MinValues: lo.ToPtr(2)}),
			Entry(greaterThan1OperatorWithFlexibility, greaterThan1OperatorWithFlexibility, greaterThan1OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, greaterThan9OperatorWithFlexibility, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, lessThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan1OperatorWithFlexibility, lessThan9OperatorWithFlexibility, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, lessThan: lessThan9.lessThan, values: sets.New[string](), MinValues: lo.ToPtr(1)}),

			Entry(greaterThan9OperatorWithFlexibility, existsOperatorWithFlexibility, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(greaterThan9OperatorWithFlexibility, notInAOperatorWithFlexibility, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, in1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, in9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(greaterThan9OperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: true, greaterThan: greaterThan9.greaterThan, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(greaterThan9OperatorWithFlexibility, greaterThan1OperatorWithFlexibility, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, greaterThan9OperatorWithFlexibility, greaterThan9OperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, lessThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(greaterThan9OperatorWithFlexibility, lessThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),

			Entry(lessThan1OperatorWithFlexibility, existsOperatorWithFlexibility, lessThan1OperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(lessThan1OperatorWithFlexibility, notInAOperatorWithFlexibility, lessThan1OperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, in1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, in9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(lessThan1OperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: true, lessThan: lessThan1.lessThan, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(lessThan1OperatorWithFlexibility, greaterThan1OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, greaterThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, lessThan1OperatorWithFlexibility, lessThan1OperatorWithFlexibility),
			Entry(lessThan1OperatorWithFlexibility, lessThan9OperatorWithFlexibility, lessThan1OperatorWithFlexibility),

			Entry(lessThan9OperatorWithFlexibility, existsOperatorWithFlexibility, lessThan9OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, inAOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, inBOperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, inABOperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.Set[string]{}, MinValues: lo.ToPtr(2)}),
			Entry(lessThan9OperatorWithFlexibility, notInAOperatorWithFlexibility, lessThan9OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, in1OperatorWithFlexibility, in1OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, in9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, in19OperatorWithFlexibility, &Requirement{Key: "key", complement: false, values: sets.New("1"), MinValues: lo.ToPtr(2)}),
			Entry(lessThan9OperatorWithFlexibility, notIn12OperatorWithFlexibility, &Requirement{Key: "key", complement: true, lessThan: lessThan9.lessThan, values: sets.New("1", "2"), MinValues: lo.ToPtr(2)}),
			Entry(lessThan9OperatorWithFlexibility, greaterThan1OperatorWithFlexibility, &Requirement{Key: "key", complement: true, greaterThan: greaterThan1.greaterThan, lessThan: lessThan9.lessThan, values: sets.New[string](), MinValues: lo.ToPtr(1)}),
			Entry(lessThan9OperatorWithFlexibility, greaterThan9OperatorWithFlexibility, doesNotExistOperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, lessThan1OperatorWithFlexibility, lessThan1OperatorWithFlexibility),
			Entry(lessThan9OperatorWithFlexibility, lessThan9OperatorWithFlexibility, lessThan9OperatorWithFlexibility),
		)
	})
	Context("Has", func() {
		It("should have the right values", func() {
			Expect(exists.Has("A")).To(BeTrue())
			Expect(doesNotExist.Has("A")).To(BeFalse())
			Expect(inA.Has("A")).To(BeTrue())
			Expect(inB.Has("A")).To(BeFalse())
			Expect(inAB.Has("A")).To(BeTrue())
			Expect(notInA.Has("A")).To(BeFalse())
			Expect(in1.Has("A")).To(BeFalse())
			Expect(in9.Has("A")).To(BeFalse())
			Expect(in19.Has("A")).To(BeFalse())
			Expect(notIn12.Has("A")).To(BeTrue())
			Expect(greaterThan1.Has("A")).To(BeFalse())
			Expect(greaterThan9.Has("A")).To(BeFalse())
			Expect(lessThan1.Has("A")).To(BeFalse())
			Expect(lessThan9.Has("A")).To(BeFalse())

			Expect(exists.Has("B")).To(BeTrue())
			Expect(doesNotExist.Has("B")).To(BeFalse())
			Expect(inA.Has("B")).To(BeFalse())
			Expect(inB.Has("B")).To(BeTrue())
			Expect(inAB.Has("B")).To(BeTrue())
			Expect(notInA.Has("B")).To(BeTrue())
			Expect(in1.Has("B")).To(BeFalse())
			Expect(in9.Has("B")).To(BeFalse())
			Expect(in19.Has("B")).To(BeFalse())
			Expect(notIn12.Has("B")).To(BeTrue())
			Expect(greaterThan1.Has("B")).To(BeFalse())
			Expect(greaterThan9.Has("B")).To(BeFalse())
			Expect(lessThan1.Has("B")).To(BeFalse())
			Expect(lessThan9.Has("B")).To(BeFalse())

			Expect(exists.Has("1")).To(BeTrue())
			Expect(doesNotExist.Has("1")).To(BeFalse())
			Expect(inA.Has("1")).To(BeFalse())
			Expect(inB.Has("1")).To(BeFalse())
			Expect(inAB.Has("1")).To(BeFalse())
			Expect(notInA.Has("1")).To(BeTrue())
			Expect(in1.Has("1")).To(BeTrue())
			Expect(in9.Has("1")).To(BeFalse())
			Expect(in19.Has("1")).To(BeTrue())
			Expect(notIn12.Has("1")).To(BeFalse())
			Expect(greaterThan1.Has("1")).To(BeFalse())
			Expect(greaterThan9.Has("1")).To(BeFalse())
			Expect(lessThan1.Has("1")).To(BeFalse())
			Expect(lessThan9.Has("1")).To(BeTrue())

			Expect(exists.Has("2")).To(BeTrue())
			Expect(doesNotExist.Has("2")).To(BeFalse())
			Expect(inA.Has("2")).To(BeFalse())
			Expect(inB.Has("2")).To(BeFalse())
			Expect(inAB.Has("2")).To(BeFalse())
			Expect(notInA.Has("2")).To(BeTrue())
			Expect(in1.Has("2")).To(BeFalse())
			Expect(in9.Has("2")).To(BeFalse())
			Expect(in19.Has("2")).To(BeFalse())
			Expect(notIn12.Has("2")).To(BeFalse())
			Expect(greaterThan1.Has("2")).To(BeTrue())
			Expect(greaterThan9.Has("2")).To(BeFalse())
			Expect(lessThan1.Has("2")).To(BeFalse())
			Expect(lessThan9.Has("2")).To(BeTrue())

			Expect(exists.Has("9")).To(BeTrue())
			Expect(doesNotExist.Has("9")).To(BeFalse())
			Expect(inA.Has("9")).To(BeFalse())
			Expect(inB.Has("9")).To(BeFalse())
			Expect(inAB.Has("9")).To(BeFalse())
			Expect(notInA.Has("9")).To(BeTrue())
			Expect(in1.Has("9")).To(BeFalse())
			Expect(in9.Has("9")).To(BeTrue())
			Expect(in19.Has("9")).To(BeTrue())
			Expect(notIn12.Has("9")).To(BeTrue())
			Expect(greaterThan1.Has("9")).To(BeTrue())
			Expect(greaterThan9.Has("9")).To(BeFalse())
			Expect(lessThan1.Has("9")).To(BeFalse())
			Expect(lessThan9.Has("9")).To(BeFalse())
		})
	})
	Context("Operator", func() {
		It("should return the right operator", func() {
			Expect(exists.Operator()).To(Equal(v1.NodeSelectorOpExists))
			Expect(doesNotExist.Operator()).To(Equal(v1.NodeSelectorOpDoesNotExist))
			Expect(inA.Operator()).To(Equal(v1.NodeSelectorOpIn))
			Expect(inB.Operator()).To(Equal(v1.NodeSelectorOpIn))
			Expect(inAB.Operator()).To(Equal(v1.NodeSelectorOpIn))
			Expect(notInA.Operator()).To(Equal(v1.NodeSelectorOpNotIn))
			Expect(in1.Operator()).To(Equal(v1.NodeSelectorOpIn))
			Expect(in9.Operator()).To(Equal(v1.NodeSelectorOpIn))
			Expect(in19.Operator()).To(Equal(v1.NodeSelectorOpIn))
			Expect(notIn12.Operator()).To(Equal(v1.NodeSelectorOpNotIn))
			Expect(greaterThan1.Operator()).To(Equal(v1.NodeSelectorOpExists))
			Expect(greaterThan9.Operator()).To(Equal(v1.NodeSelectorOpExists))
			Expect(lessThan1.Operator()).To(Equal(v1.NodeSelectorOpExists))
			Expect(lessThan9.Operator()).To(Equal(v1.NodeSelectorOpExists))
		})
	})
	Context("Len", func() {
		It("should have the correct length", func() {
			Expect(exists.Len()).To(Equal(math.MaxInt64))
			Expect(doesNotExist.Len()).To(Equal(0))
			Expect(inA.Len()).To(Equal(1))
			Expect(inB.Len()).To(Equal(1))
			Expect(inAB.Len()).To(Equal(2))
			Expect(notInA.Len()).To(Equal(math.MaxInt64 - 1))
			Expect(in1.Len()).To(Equal(1))
			Expect(in9.Len()).To(Equal(1))
			Expect(in19.Len()).To(Equal(2))
			Expect(notIn12.Len()).To(Equal(math.MaxInt64 - 2))
			Expect(greaterThan1.Len()).To(Equal(math.MaxInt64))
			Expect(greaterThan9.Len()).To(Equal(math.MaxInt64))
			Expect(lessThan1.Len()).To(Equal(math.MaxInt64))
			Expect(lessThan9.Len()).To(Equal(math.MaxInt64))
		})
	})
	Context("Any", func() {
		It("should return any", func() {
			Expect(exists.Any()).ToNot(BeEmpty())
			Expect(doesNotExist.Any()).To(BeEmpty())
			Expect(inA.Any()).To(Equal("A"))
			Expect(inB.Any()).To(Equal("B"))
			Expect(inAB.Any()).To(Or(Equal("A"), Equal("B")))
			Expect(notInA.Any()).ToNot(Or(BeEmpty(), Equal("A")))
			Expect(in1.Any()).To(Equal("1"))
			Expect(in9.Any()).To(Equal("9"))
			Expect(in19.Any()).To(Or(Equal("1"), Equal("9")))
			Expect(notIn12.Any()).ToNot(Or(BeEmpty(), Equal("1"), Equal("2")))
			Expect(strconv.Atoi(greaterThan1.Any())).To(BeNumerically(">=", 1))
			Expect(strconv.Atoi(greaterThan9.Any())).To(And(BeNumerically(">=", 9), BeNumerically("<", math.MaxInt64)))
			Expect(lessThan1.Any()).To(Equal("0"))
			Expect(strconv.Atoi(lessThan9.Any())).To(And(BeNumerically(">=", 0), BeNumerically("<", 9)))
		})
	})
	Context("String", func() {
		It("should print the right string", func() {
			Expect(exists.String()).To(Equal("key Exists"))
			Expect(doesNotExist.String()).To(Equal("key DoesNotExist"))
			Expect(inA.String()).To(Equal("key In [A]"))
			Expect(inB.String()).To(Equal("key In [B]"))
			Expect(inAB.String()).To(Equal("key In [A B]"))
			Expect(notInA.String()).To(Equal("key NotIn [A]"))
			Expect(in1.String()).To(Equal("key In [1]"))
			Expect(in9.String()).To(Equal("key In [9]"))
			Expect(in19.String()).To(Equal("key In [1 9]"))
			Expect(notIn12.String()).To(Equal("key NotIn [1 2]"))
			Expect(greaterThan1.String()).To(Equal("key Exists >1"))
			Expect(greaterThan9.String()).To(Equal("key Exists >9"))
			Expect(lessThan1.String()).To(Equal("key Exists <1"))
			Expect(lessThan9.String()).To(Equal("key Exists <9"))
			Expect(greaterThan1.Intersection(lessThan9).String()).To(Equal("key Exists >1 <9"))
			Expect(greaterThan9.Intersection(lessThan1).String()).To(Equal("key DoesNotExist"))
		})
	})
	Context("NodeSelectorRequirements Conversion", func() {
		It("should return the expected NodeSelectorRequirement", func() {
			Expect(exists.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpExists}}))
			Expect(doesNotExist.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpDoesNotExist}}))
			Expect(inA.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"A"}}}))
			Expect(inB.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"B"}}}))
			Expect(inAB.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"A", "B"}}}))
			Expect(notInA.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"A"}}}))
			Expect(in1.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"1"}}}))
			Expect(in9.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"9"}}}))
			Expect(in19.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"1", "9"}}}))
			Expect(notIn12.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"1", "2"}}}))
			Expect(greaterThan1.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpGt, Values: []string{"1"}}}))
			Expect(greaterThan9.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpGt, Values: []string{"9"}}}))
			Expect(lessThan1.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpLt, Values: []string{"1"}}}))
			Expect(lessThan9.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpLt, Values: []string{"9"}}}))

			Expect(existsOperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpExists}, MinValues: lo.ToPtr(1)}))
			Expect(doesNotExistOperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpDoesNotExist}, MinValues: lo.ToPtr(1)}))
			Expect(inAOperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"A"}}, MinValues: lo.ToPtr(1)}))
			Expect(inBOperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"B"}}, MinValues: lo.ToPtr(1)}))
			Expect(inABOperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"A", "B"}}, MinValues: lo.ToPtr(2)}))
			Expect(notInAOperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"A"}}, MinValues: lo.ToPtr(1)}))
			Expect(in1OperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"1"}}, MinValues: lo.ToPtr(1)}))
			Expect(in9OperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"9"}}, MinValues: lo.ToPtr(1)}))
			Expect(in19OperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpIn, Values: []string{"1", "9"}}, MinValues: lo.ToPtr(2)}))
			Expect(notIn12OperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"1", "2"}}, MinValues: lo.ToPtr(2)}))
			Expect(greaterThan1OperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpGt, Values: []string{"1"}}, MinValues: lo.ToPtr(1)}))
			Expect(greaterThan9OperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpGt, Values: []string{"9"}}, MinValues: lo.ToPtr(1)}))
			Expect(lessThan1OperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpLt, Values: []string{"1"}}, MinValues: lo.ToPtr(1)}))
			Expect(lessThan9OperatorWithFlexibility.NodeSelectorRequirement()).To(Equal(v1beta1.NodeSelectorRequirementWithMinValues{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "key", Operator: v1.NodeSelectorOpLt, Values: []string{"9"}}, MinValues: lo.ToPtr(1)}))
		})

	})
})
