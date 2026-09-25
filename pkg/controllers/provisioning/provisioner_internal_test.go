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

package provisioning

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func testRequirement(key string, operator corev1.NodeSelectorOperator) corev1.NodeSelectorRequirement {
	requirement := corev1.NodeSelectorRequirement{Key: key, Operator: operator}
	if operator != corev1.NodeSelectorOpExists && operator != corev1.NodeSelectorOpDoesNotExist {
		requirement.Values = []string{"1"}
	}
	return requirement
}

func testTerm(requirements ...corev1.NodeSelectorRequirement) corev1.NodeSelectorTerm {
	return corev1.NodeSelectorTerm{MatchExpressions: requirements}
}

func testRequiredPod(terms ...corev1.NodeSelectorTerm) *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms},
	}}}}
}

type affinityTermCase struct {
	name        string
	term        corev1.NodeSelectorTerm
	usable      bool
	excludes    bool
	matchFields bool
}

func checkAffinityCombination(selected []affinityTermCase) {
	GinkgoHelper()
	terms := make([]corev1.NodeSelectorTerm, 0, len(selected))
	names := make([]string, 0, len(selected))
	hasUsableTerm := false
	allUsableTermsExclude := true
	hasMatchFields := false
	for _, selectedTerm := range selected {
		terms = append(terms, selectedTerm.term)
		names = append(names, selectedTerm.name)
		hasMatchFields = hasMatchFields || selectedTerm.matchFields
		if selectedTerm.usable {
			hasUsableTerm = true
			allUsableTermsExclude = allUsableTermsExclude && selectedTerm.excludes
		}
	}
	want := !hasMatchFields && hasUsableTerm && allUsableTermsExclude

	Expect(requiresNonKarpenterNode(testRequiredPod(terms...))).To(
		Equal(want),
		"required=%v",
		names,
	)
}

func visitAffinityTermCombinations(cases []affinityTermCase, maxTerms int, visit func([]affinityTermCase)) {
	visit(nil)
	var generate func(remaining int, selected []affinityTermCase)
	generate = func(remaining int, selected []affinityTermCase) {
		if remaining == 0 {
			visit(selected)
			return
		}
		for _, candidate := range cases {
			generate(remaining-1, append(selected, candidate))
		}
	}
	for termCount := 1; termCount <= maxTerms; termCount++ {
		generate(termCount, nil)
	}
}

var _ = Describe("Provisioner Affinity Validation", func() {
	nodePoolDoesNotExist := testRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpDoesNotExist)
	nodePoolIn := testRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpIn)
	zoneIn := testRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn)

	DescribeTable("classifies whether a pod requires a non-Karpenter node",
		func(pod *corev1.Pod, want bool) {
			Expect(requiresNonKarpenterNode(pod)).To(Equal(want))
		},
		Entry("with no affinity", &corev1.Pod{}, false),
		Entry("with empty affinity",
			&corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{}}},
			false,
		),
		Entry("with empty node affinity",
			&corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}}},
			false,
		),
		Entry("when a required exclusion wins over a permissive preference",
			func() *corev1.Pod {
				pod := testRequiredPod(testTerm(nodePoolDoesNotExist))
				pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.PreferredSchedulingTerm{{
					Weight: 50, Preference: testTerm(zoneIn),
				}}
				return pod
			}(),
			true,
		),
		Entry("when a required alternative permits Karpenter despite an excluding preference",
			func() *corev1.Pod {
				pod := testRequiredPod(testTerm(zoneIn))
				pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.PreferredSchedulingTerm{{
					Weight: 50, Preference: testTerm(nodePoolDoesNotExist),
				}}
				return pod
			}(),
			false,
		),
		Entry("when a required exclusion is last in an AND term",
			testRequiredPod(testTerm(zoneIn, nodePoolDoesNotExist)),
			true,
		),
		Entry("when a duplicate contradictory key still explicitly excludes Karpenter",
			testRequiredPod(testTerm(nodePoolIn, nodePoolDoesNotExist)),
			true,
		),
		Entry("when DoesNotExist on another key does not exclude Karpenter",
			testRequiredPod(testTerm(testRequirement("example.com/other", corev1.NodeSelectorOpDoesNotExist))),
			false,
		),
		Entry("when empty matchFields is handled by generic affinity validation",
			testRequiredPod(corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{nodePoolDoesNotExist},
				MatchFields:      []corev1.NodeSelectorRequirement{},
			}),
			false,
		),
		Entry("when required pod affinity does not constrain node labels",
			&corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key: v1.NodePoolLabelKey, Operator: metav1.LabelSelectorOpDoesNotExist,
					}}},
					TopologyKey: corev1.LabelHostname,
				}},
			}}}},
			false,
		),
		Entry("when required pod anti-affinity does not constrain node labels",
			&corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key: v1.NodePoolLabelKey, Operator: metav1.LabelSelectorOpDoesNotExist,
					}}},
					TopologyKey: corev1.LabelHostname,
				}},
			}}}},
			false,
		),
	)

	DescribeTable("does not classify other NodePool operators as exclusions",
		func(operator corev1.NodeSelectorOperator) {
			requirement := testRequirement(v1.NodePoolLabelKey, operator)
			Expect(requiresNonKarpenterNode(testRequiredPod(testTerm(requirement)))).To(BeFalse())
		},
		Entry(string(corev1.NodeSelectorOpIn), corev1.NodeSelectorOpIn),
		Entry(string(corev1.NodeSelectorOpNotIn), corev1.NodeSelectorOpNotIn),
		Entry(string(corev1.NodeSelectorOpExists), corev1.NodeSelectorOpExists),
		Entry(string(corev1.NodeSelectorOpGt), corev1.NodeSelectorOpGt),
		Entry(string(corev1.NodeSelectorOpLt), corev1.NodeSelectorOpLt),
	)

	It("classifies pairwise required term combinations", func() {
		termCases := []affinityTermCase{
			{name: "empty", term: corev1.NodeSelectorTerm{}},
			{name: "nodepool DoesNotExist", term: testTerm(nodePoolDoesNotExist), usable: true, excludes: true},
			{name: "zone", term: testTerm(zoneIn), usable: true},
			{
				name: "matchFields",
				term: corev1.NodeSelectorTerm{
					MatchExpressions: []corev1.NodeSelectorRequirement{nodePoolDoesNotExist},
					MatchFields:      []corev1.NodeSelectorRequirement{testRequirement("metadata.name", corev1.NodeSelectorOpIn)},
				},
				matchFields: true,
			},
		}
		// Pairwise coverage is sufficient because required terms are a flat OR:
		// each additional term can only preserve or clear the all-terms-exclude state.
		visitAffinityTermCombinations(termCases, 2, checkAffinityCombination)
	})

	It("returns a sentinel-compatible error for a hard Karpenter exclusion", func() {
		err := validateKarpenterManagedLabelCanExist(testRequiredPod(testTerm(nodePoolDoesNotExist)))
		Expect(errors.Is(err, KarpenterManagedLabelDoesNotExistError)).To(BeTrue())
	})
})
