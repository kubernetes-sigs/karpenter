/*
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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
)

var _ = Describe("Requirements", func() {
	Context("Compatibility", func() {
		It("should normalize aliased labels", func() {
			requirements := NewRequirements(NewRequirement(v1.LabelFailureDomainBetaZone, v1.NodeSelectorOpIn, "test"))
			Expect(requirements.Has(v1.LabelFailureDomainBetaZone)).To(BeFalse())
			Expect(requirements.Get(v1.LabelTopologyZone).Has("test")).To(BeTrue())
		})

		// Use a well known label like zone, because it behaves differently than custom labels
		unconstrained := NewRequirements()
		exists := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpExists))
		doesNotExist := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpDoesNotExist))
		inA := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, "A"))
		inB := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, "B"))
		inAB := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, "A", "B"))
		notInA := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpNotIn, "A"))
		in1 := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, "1"))
		in9 := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, "9"))
		in19 := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, "1", "9"))
		notIn12 := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpNotIn, "1", "2"))
		greaterThan1 := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpGt, "1"))
		greaterThan9 := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpGt, "9"))
		lessThan1 := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpLt, "1"))
		lessThan9 := NewRequirements(NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpLt, "9"))

		It("should be compatible", func() {
			Expect(unconstrained.Compatible(unconstrained)).To(Succeed())
			Expect(unconstrained.Compatible(exists)).To(Succeed())
			Expect(unconstrained.Compatible(doesNotExist)).To(Succeed())
			Expect(unconstrained.Compatible(inA)).To(Succeed())
			Expect(unconstrained.Compatible(inB)).To(Succeed())
			Expect(unconstrained.Compatible(inAB)).To(Succeed())
			Expect(unconstrained.Compatible(notInA)).To(Succeed())
			Expect(unconstrained.Compatible(in1)).To(Succeed())
			Expect(unconstrained.Compatible(in9)).To(Succeed())
			Expect(unconstrained.Compatible(in19)).To(Succeed())
			Expect(unconstrained.Compatible(notIn12)).To(Succeed())
			Expect(unconstrained.Compatible(greaterThan1)).To(Succeed())
			Expect(unconstrained.Compatible(greaterThan9)).To(Succeed())
			Expect(unconstrained.Compatible(lessThan1)).To(Succeed())
			Expect(unconstrained.Compatible(lessThan9)).To(Succeed())

			Expect(exists.Compatible(unconstrained)).To(Succeed())
			Expect(exists.Compatible(exists)).To(Succeed())
			Expect(exists.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(exists.Compatible(inA)).To(Succeed())
			Expect(exists.Compatible(inB)).To(Succeed())
			Expect(exists.Compatible(inAB)).To(Succeed())
			Expect(exists.Compatible(notInA)).To(Succeed())
			Expect(exists.Compatible(in1)).To(Succeed())
			Expect(exists.Compatible(in9)).To(Succeed())
			Expect(exists.Compatible(in19)).To(Succeed())
			Expect(exists.Compatible(notIn12)).To(Succeed())
			Expect(exists.Compatible(greaterThan1)).To(Succeed())
			Expect(exists.Compatible(greaterThan9)).To(Succeed())
			Expect(exists.Compatible(lessThan1)).To(Succeed())
			Expect(exists.Compatible(lessThan9)).To(Succeed())

			Expect(doesNotExist.Compatible(unconstrained)).To(Succeed())
			Expect(doesNotExist.Compatible(exists)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(doesNotExist)).To(Succeed())
			Expect(doesNotExist.Compatible(inA)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(inB)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(inAB)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(notInA)).To(Succeed())
			Expect(doesNotExist.Compatible(in1)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(in9)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(in19)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(notIn12)).To(Succeed())
			Expect(doesNotExist.Compatible(greaterThan1)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(lessThan1)).ToNot(Succeed())
			Expect(doesNotExist.Compatible(lessThan9)).ToNot(Succeed())

			Expect(inA.Compatible(unconstrained)).To(Succeed())
			Expect(inA.Compatible(exists)).To(Succeed())
			Expect(inA.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(inA.Compatible(inA)).To(Succeed())
			Expect(inA.Compatible(inB)).ToNot(Succeed())
			Expect(inA.Compatible(inAB)).To(Succeed())
			Expect(inA.Compatible(notInA)).ToNot(Succeed())
			Expect(inA.Compatible(in1)).ToNot(Succeed())
			Expect(inA.Compatible(in9)).ToNot(Succeed())
			Expect(inA.Compatible(in19)).ToNot(Succeed())
			Expect(inA.Compatible(notIn12)).To(Succeed())
			Expect(inA.Compatible(greaterThan1)).ToNot(Succeed())
			Expect(inA.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(inA.Compatible(lessThan1)).ToNot(Succeed())
			Expect(inA.Compatible(lessThan9)).ToNot(Succeed())

			Expect(inB.Compatible(unconstrained)).To(Succeed())
			Expect(inB.Compatible(exists)).To(Succeed())
			Expect(inB.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(inB.Compatible(inA)).ToNot(Succeed())
			Expect(inB.Compatible(inB)).To(Succeed())
			Expect(inB.Compatible(inAB)).To(Succeed())
			Expect(inB.Compatible(notInA)).To(Succeed())
			Expect(inB.Compatible(in1)).ToNot(Succeed())
			Expect(inB.Compatible(in9)).ToNot(Succeed())
			Expect(inB.Compatible(in19)).ToNot(Succeed())
			Expect(inB.Compatible(notIn12)).To(Succeed())
			Expect(inB.Compatible(greaterThan1)).ToNot(Succeed())
			Expect(inB.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(inB.Compatible(lessThan1)).ToNot(Succeed())
			Expect(inB.Compatible(lessThan9)).ToNot(Succeed())

			Expect(inAB.Compatible(unconstrained)).To(Succeed())
			Expect(inAB.Compatible(exists)).To(Succeed())
			Expect(inAB.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(inAB.Compatible(inA)).To(Succeed())
			Expect(inAB.Compatible(inB)).To(Succeed())
			Expect(inAB.Compatible(inAB)).To(Succeed())
			Expect(inAB.Compatible(notInA)).To(Succeed())
			Expect(inAB.Compatible(in1)).ToNot(Succeed())
			Expect(inAB.Compatible(in9)).ToNot(Succeed())
			Expect(inAB.Compatible(in19)).ToNot(Succeed())
			Expect(inAB.Compatible(notIn12)).To(Succeed())
			Expect(inAB.Compatible(greaterThan1)).ToNot(Succeed())
			Expect(inAB.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(inAB.Compatible(lessThan1)).ToNot(Succeed())
			Expect(inAB.Compatible(lessThan9)).ToNot(Succeed())

			Expect(notInA.Compatible(unconstrained)).To(Succeed())
			Expect(notInA.Compatible(exists)).To(Succeed())
			Expect(notInA.Compatible(doesNotExist)).To(Succeed())
			Expect(notInA.Compatible(inA)).ToNot(Succeed())
			Expect(notInA.Compatible(inB)).To(Succeed())
			Expect(notInA.Compatible(inAB)).To(Succeed())
			Expect(notInA.Compatible(notInA)).To(Succeed())
			Expect(notInA.Compatible(in1)).To(Succeed())
			Expect(notInA.Compatible(in9)).To(Succeed())
			Expect(notInA.Compatible(in19)).To(Succeed())
			Expect(notInA.Compatible(notIn12)).To(Succeed())
			Expect(notInA.Compatible(greaterThan1)).To(Succeed())
			Expect(notInA.Compatible(greaterThan9)).To(Succeed())
			Expect(notInA.Compatible(lessThan1)).To(Succeed())
			Expect(notInA.Compatible(lessThan9)).To(Succeed())

			Expect(in1.Compatible(unconstrained)).To(Succeed())
			Expect(in1.Compatible(exists)).To(Succeed())
			Expect(in1.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(in1.Compatible(inA)).ToNot(Succeed())
			Expect(in1.Compatible(inB)).ToNot(Succeed())
			Expect(in1.Compatible(inAB)).ToNot(Succeed())
			Expect(in1.Compatible(notInA)).To(Succeed())
			Expect(in1.Compatible(in1)).To(Succeed())
			Expect(in1.Compatible(in9)).ToNot(Succeed())
			Expect(in1.Compatible(in19)).To(Succeed())
			Expect(in1.Compatible(notIn12)).ToNot(Succeed())
			Expect(in1.Compatible(greaterThan1)).ToNot(Succeed())
			Expect(in1.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(in1.Compatible(lessThan1)).ToNot(Succeed())
			Expect(in1.Compatible(lessThan9)).To(Succeed())

			Expect(in9.Compatible(unconstrained)).To(Succeed())
			Expect(in9.Compatible(exists)).To(Succeed())
			Expect(in9.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(in9.Compatible(inA)).ToNot(Succeed())
			Expect(in9.Compatible(inB)).ToNot(Succeed())
			Expect(in9.Compatible(inAB)).ToNot(Succeed())
			Expect(in9.Compatible(notInA)).To(Succeed())
			Expect(in9.Compatible(in1)).ToNot(Succeed())
			Expect(in9.Compatible(in9)).To(Succeed())
			Expect(in9.Compatible(in19)).To(Succeed())
			Expect(in9.Compatible(notIn12)).To(Succeed())
			Expect(in9.Compatible(greaterThan1)).To(Succeed())
			Expect(in9.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(in9.Compatible(lessThan1)).ToNot(Succeed())
			Expect(in9.Compatible(lessThan9)).ToNot(Succeed())

			Expect(in19.Compatible(unconstrained)).To(Succeed())
			Expect(in19.Compatible(exists)).To(Succeed())
			Expect(in19.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(in19.Compatible(inA)).ToNot(Succeed())
			Expect(in19.Compatible(inB)).ToNot(Succeed())
			Expect(in19.Compatible(inAB)).ToNot(Succeed())
			Expect(in19.Compatible(notInA)).To(Succeed())
			Expect(in19.Compatible(in1)).To(Succeed())
			Expect(in19.Compatible(in9)).To(Succeed())
			Expect(in19.Compatible(in19)).To(Succeed())
			Expect(in19.Compatible(notIn12)).To(Succeed())
			Expect(in19.Compatible(greaterThan1)).To(Succeed())
			Expect(in19.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(in19.Compatible(lessThan1)).ToNot(Succeed())
			Expect(in19.Compatible(lessThan9)).To(Succeed())

			Expect(notIn12.Compatible(unconstrained)).To(Succeed())
			Expect(notIn12.Compatible(exists)).To(Succeed())
			Expect(notIn12.Compatible(doesNotExist)).To(Succeed())
			Expect(notIn12.Compatible(inA)).To(Succeed())
			Expect(notIn12.Compatible(inB)).To(Succeed())
			Expect(notIn12.Compatible(inAB)).To(Succeed())
			Expect(notIn12.Compatible(notInA)).To(Succeed())
			Expect(notIn12.Compatible(in1)).ToNot(Succeed())
			Expect(notIn12.Compatible(in9)).To(Succeed())
			Expect(notIn12.Compatible(in19)).To(Succeed())
			Expect(notIn12.Compatible(notIn12)).To(Succeed())
			Expect(notIn12.Compatible(greaterThan1)).To(Succeed())
			Expect(notIn12.Compatible(greaterThan9)).To(Succeed())
			Expect(notIn12.Compatible(lessThan1)).To(Succeed())
			Expect(notIn12.Compatible(lessThan9)).To(Succeed())

			Expect(greaterThan1.Compatible(unconstrained)).To(Succeed())
			Expect(greaterThan1.Compatible(exists)).To(Succeed())
			Expect(greaterThan1.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(greaterThan1.Compatible(inA)).ToNot(Succeed())
			Expect(greaterThan1.Compatible(inB)).ToNot(Succeed())
			Expect(greaterThan1.Compatible(inAB)).ToNot(Succeed())
			Expect(greaterThan1.Compatible(notInA)).To(Succeed())
			Expect(greaterThan1.Compatible(in1)).ToNot(Succeed())
			Expect(greaterThan1.Compatible(in9)).To(Succeed())
			Expect(greaterThan1.Compatible(in19)).To(Succeed())
			Expect(greaterThan1.Compatible(notIn12)).To(Succeed())
			Expect(greaterThan1.Compatible(greaterThan1)).To(Succeed())
			Expect(greaterThan1.Compatible(greaterThan9)).To(Succeed())
			Expect(greaterThan1.Compatible(lessThan1)).ToNot(Succeed())
			Expect(greaterThan1.Compatible(lessThan9)).To(Succeed())

			Expect(greaterThan9.Compatible(unconstrained)).To(Succeed())
			Expect(greaterThan9.Compatible(exists)).To(Succeed())
			Expect(greaterThan9.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(greaterThan9.Compatible(inA)).ToNot(Succeed())
			Expect(greaterThan9.Compatible(inB)).ToNot(Succeed())
			Expect(greaterThan9.Compatible(inAB)).ToNot(Succeed())
			Expect(greaterThan9.Compatible(notInA)).To(Succeed())
			Expect(greaterThan9.Compatible(in1)).ToNot(Succeed())
			Expect(greaterThan9.Compatible(in9)).ToNot(Succeed())
			Expect(greaterThan9.Compatible(in19)).ToNot(Succeed())
			Expect(greaterThan9.Compatible(notIn12)).To(Succeed())
			Expect(greaterThan9.Compatible(greaterThan1)).To(Succeed())
			Expect(greaterThan9.Compatible(greaterThan9)).To(Succeed())
			Expect(greaterThan9.Compatible(lessThan1)).ToNot(Succeed())
			Expect(greaterThan9.Compatible(lessThan9)).ToNot(Succeed())

			Expect(lessThan1.Compatible(unconstrained)).To(Succeed())
			Expect(lessThan1.Compatible(exists)).To(Succeed())
			Expect(lessThan1.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(lessThan1.Compatible(inA)).ToNot(Succeed())
			Expect(lessThan1.Compatible(inB)).ToNot(Succeed())
			Expect(lessThan1.Compatible(inAB)).ToNot(Succeed())
			Expect(lessThan1.Compatible(notInA)).To(Succeed())
			Expect(lessThan1.Compatible(in1)).ToNot(Succeed())
			Expect(lessThan1.Compatible(in9)).ToNot(Succeed())
			Expect(lessThan1.Compatible(in19)).ToNot(Succeed())
			Expect(lessThan1.Compatible(notIn12)).To(Succeed())
			Expect(lessThan1.Compatible(greaterThan1)).ToNot(Succeed())
			Expect(lessThan1.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(lessThan1.Compatible(lessThan1)).To(Succeed())
			Expect(lessThan1.Compatible(lessThan9)).To(Succeed())

			Expect(lessThan9.Compatible(unconstrained)).To(Succeed())
			Expect(lessThan9.Compatible(exists)).To(Succeed())
			Expect(lessThan9.Compatible(doesNotExist)).ToNot(Succeed())
			Expect(lessThan9.Compatible(inA)).ToNot(Succeed())
			Expect(lessThan9.Compatible(inB)).ToNot(Succeed())
			Expect(lessThan9.Compatible(inAB)).ToNot(Succeed())
			Expect(lessThan9.Compatible(notInA)).To(Succeed())
			Expect(lessThan9.Compatible(in1)).To(Succeed())
			Expect(lessThan9.Compatible(in9)).ToNot(Succeed())
			Expect(lessThan9.Compatible(in19)).To(Succeed())
			Expect(lessThan9.Compatible(notIn12)).To(Succeed())
			Expect(lessThan9.Compatible(greaterThan1)).To(Succeed())
			Expect(lessThan9.Compatible(greaterThan9)).ToNot(Succeed())
			Expect(lessThan9.Compatible(lessThan1)).To(Succeed())
			Expect(lessThan9.Compatible(lessThan9)).To(Succeed())
		})
	})
	Context("Error Messages", func() {
		It("should detect well known label truncations", func() {
			unconstrained := NewRequirements()
			for _, tc := range []struct {
				badLabel      string
				expectedError string
			}{
				{
					badLabel:      "zone",
					expectedError: `label "zone" does not have known values (typo of "topology.kubernetes.io/zone"?)`,
				},
				{
					badLabel:      "region",
					expectedError: `label "region" does not have known values (typo of "topology.kubernetes.io/region"?)`,
				},
				{
					badLabel:      "provisioner-name",
					expectedError: `label "provisioner-name" does not have known values (typo of "karpenter.sh/provisioner-name"?)`,
				},
				{
					badLabel:      "instance-type",
					expectedError: `label "instance-type" does not have known values (typo of "node.kubernetes.io/instance-type"?)`,
				},
				{
					badLabel:      "arch",
					expectedError: `label "arch" does not have known values (typo of "kubernetes.io/arch"?)`,
				},
				{
					badLabel:      "capacity-type",
					expectedError: `label "capacity-type" does not have known values (typo of "karpenter.sh/capacity-type"?)`,
				},
			} {
				provisionerRequirement := NewRequirements(NewRequirement(tc.badLabel, v1.NodeSelectorOpExists))
				Expect(unconstrained.Compatible(provisionerRequirement).Error()).To(Equal(tc.expectedError))
			}
		})
		It("should detect well known label typos", func() {
			unconstrained := NewRequirements()
			for _, tc := range []struct {
				badLabel      string
				expectedError string
			}{
				{
					badLabel:      "topology.kubernetesio/zone",
					expectedError: `label "topology.kubernetesio/zone" does not have known values (typo of "topology.kubernetes.io/zone"?)`,
				},
				{
					badLabel:      "topology.kubernetes.io/regio",
					expectedError: `label "topology.kubernetes.io/regio" does not have known values (typo of "topology.kubernetes.io/region"?)`,
				},
				{
					badLabel:      "karpenterprovisioner-name",
					expectedError: `label "karpenterprovisioner-name" does not have known values (typo of "karpenter.sh/provisioner-name"?)`,
				},
			} {
				provisionerRequirement := NewRequirements(NewRequirement(tc.badLabel, v1.NodeSelectorOpExists))
				Expect(unconstrained.Compatible(provisionerRequirement).Error()).To(Equal(tc.expectedError))
			}
		})
		It("should display an error message for unknown labels", func() {
			unconstrained := NewRequirements()
			provisionerRequirement := NewRequirements(NewRequirement("deployment", v1.NodeSelectorOpExists))
			Expect(unconstrained.Compatible(provisionerRequirement).Error()).To(Equal(`label "deployment" does not have known values`))
		})
	})
	Context("NodeSelectorRequirements Conversion", func() {
		It("should convert combinations of labels to expected NodeSelectorRequirements", func() {
			exists := NewRequirement("exists", v1.NodeSelectorOpExists)
			doesNotExist := NewRequirement("doesNotExist", v1.NodeSelectorOpDoesNotExist)
			inA := NewRequirement("inA", v1.NodeSelectorOpIn, "A")
			inB := NewRequirement("inB", v1.NodeSelectorOpIn, "B")
			inAB := NewRequirement("inAB", v1.NodeSelectorOpIn, "A", "B")
			notInA := NewRequirement("notInA", v1.NodeSelectorOpNotIn, "A")
			in1 := NewRequirement("in1", v1.NodeSelectorOpIn, "1")
			in9 := NewRequirement("in9", v1.NodeSelectorOpIn, "9")
			in19 := NewRequirement("in19", v1.NodeSelectorOpIn, "1", "9")
			notIn12 := NewRequirement("notIn12", v1.NodeSelectorOpNotIn, "1", "2")
			greaterThan1 := NewRequirement("greaterThan1", v1.NodeSelectorOpGt, "1")
			greaterThan9 := NewRequirement("greaterThan9", v1.NodeSelectorOpGt, "9")
			lessThan1 := NewRequirement("lessThan1", v1.NodeSelectorOpLt, "1")
			lessThan9 := NewRequirement("lessThan9", v1.NodeSelectorOpLt, "9")

			reqs := NewRequirements(
				exists,
				doesNotExist,
				inA,
				inB,
				inAB,
				notInA,
				in1,
				in9,
				in19,
				notIn12,
				greaterThan1,
				greaterThan9,
				lessThan1,
				lessThan9,
			)
			Expect(reqs.NodeSelectorRequirements()).To(ContainElements(
				v1.NodeSelectorRequirement{Key: "exists", Operator: v1.NodeSelectorOpExists},
				v1.NodeSelectorRequirement{Key: "doesNotExist", Operator: v1.NodeSelectorOpDoesNotExist},
				v1.NodeSelectorRequirement{Key: "inA", Operator: v1.NodeSelectorOpIn, Values: []string{"A"}},
				v1.NodeSelectorRequirement{Key: "inB", Operator: v1.NodeSelectorOpIn, Values: []string{"B"}},
				v1.NodeSelectorRequirement{Key: "inAB", Operator: v1.NodeSelectorOpIn, Values: []string{"A", "B"}},
				v1.NodeSelectorRequirement{Key: "notInA", Operator: v1.NodeSelectorOpNotIn, Values: []string{"A"}},
				v1.NodeSelectorRequirement{Key: "in1", Operator: v1.NodeSelectorOpIn, Values: []string{"1"}},
				v1.NodeSelectorRequirement{Key: "in9", Operator: v1.NodeSelectorOpIn, Values: []string{"9"}},
				v1.NodeSelectorRequirement{Key: "in19", Operator: v1.NodeSelectorOpIn, Values: []string{"1", "9"}},
				v1.NodeSelectorRequirement{Key: "notIn12", Operator: v1.NodeSelectorOpNotIn, Values: []string{"1", "2"}},
				v1.NodeSelectorRequirement{Key: "greaterThan1", Operator: v1.NodeSelectorOpGt, Values: []string{"1"}},
				v1.NodeSelectorRequirement{Key: "greaterThan9", Operator: v1.NodeSelectorOpGt, Values: []string{"9"}},
				v1.NodeSelectorRequirement{Key: "lessThan1", Operator: v1.NodeSelectorOpLt, Values: []string{"1"}},
				v1.NodeSelectorRequirement{Key: "lessThan9", Operator: v1.NodeSelectorOpLt, Values: []string{"9"}},
			))
			Expect(len(reqs.NodeSelectorRequirements())).To(Equal(14))
		})
	})
})

// Keeping this in case we need it, I ran for 1m+ samples and had no issues
// fuzz: elapsed: 2m27s, execs: 1002748 (6130/sec), new interesting: 30 (total: 33)
func FuzzEditDistance(f *testing.F) {
	f.Add("foo", "bar")
	f.Add("foo", "")
	f.Add("", "foo")
	f.Fuzz(func(t *testing.T, lhs, rhs string) {
		editDistance(lhs, rhs)
	})
}
