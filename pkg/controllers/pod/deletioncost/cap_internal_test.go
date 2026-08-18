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

package deletioncost

import (
	"math"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ranks builds a NodeRank slice from the given rank pattern; used to drive
// capNodeRanks table entries without repeating the struct literal.
func ranks(pattern ...int) []NodeRank {
	out := make([]NodeRank, 0, len(pattern))
	for _, r := range pattern {
		out = append(out, NodeRank{Rank: r})
	}
	return out
}

// podWithCost returns a pod with pod-deletion-cost=value. Empty value means
// no annotation set.
func podWithCost(value string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{}}
	if value != "" {
		pod.Annotations = map[string]string{corev1.PodDeletionCost: value}
	}
	return pod
}

// deletingPod returns a pod with a non-nil DeletionTimestamp (terminating).
func deletingPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{Time: time.Unix(0, 0)}}}
}

// livePod returns a pod without a DeletionTimestamp.
func livePod() *corev1.Pod {
	return &corev1.Pod{}
}

// Internal-package Ginkgo specs for the deletioncost helpers that access
// unexported symbols (capNodeRanks, filterNoOpNodes, livePodCount, NodeRank).
// The dot import shares Ginkgo's global spec registry with the external
// suite_test.go, so these run under the same RunSpecs entrypoint.
var _ = Describe("capNodeRanks", func() {
	DescribeTable("admits every Group A node and caps only the tail",
		func(input []NodeRank, limit, expectedLen int) {
			got := capNodeRanks(input, limit)
			Expect(got).To(HaveLen(expectedLen))
			// Verify the Group A prefix is preserved intact.
			groupACount := 0
			for _, r := range input {
				if r.Rank != math.MinInt32 {
					break
				}
				groupACount++
			}
			for i := 0; i < groupACount && i < len(got); i++ {
				Expect(got[i].Rank).To(Equal(int(math.MinInt32)), "position %d: expected Group A sentinel", i)
			}
		},
		Entry("3 Group A + 5 tail with cap=2 → 3 A + 2 tail",
			ranks(math.MinInt32, math.MinInt32, math.MinInt32, -5, -4, -3, -2, -1), 2, 5),
		Entry("100 Group A alone with cap=50 → all 100 retained",
			func() []NodeRank {
				in := make([]NodeRank, 100)
				for i := range in {
					in[i] = NodeRank{Rank: math.MinInt32}
				}
				return in
			}(), 50, 100),
		Entry("100 Group A + 100 tail with cap=50 → 100 A + 50 tail",
			func() []NodeRank {
				in := make([]NodeRank, 0, 200)
				for i := 0; i < 100; i++ {
					in = append(in, NodeRank{Rank: math.MinInt32})
				}
				for i := 0; i < 100; i++ {
					in = append(in, NodeRank{Rank: -100 + i})
				}
				return in
			}(), 50, 150),
		Entry("tail below cap → all input retained",
			ranks(math.MinInt32, -3, -2, -1), 50, 4),
		Entry("empty input → empty output",
			[]NodeRank(nil), 10, 0),
		Entry("no Group A → cap applies from the start",
			ranks(-5, -4, -3, -2, -1), 3, 3),
		Entry("30 Group A + 30 tail with cap=50 → tail below cap, all 60 retained",
			func() []NodeRank {
				in := make([]NodeRank, 0, 60)
				for i := 0; i < 30; i++ {
					in = append(in, NodeRank{Rank: math.MinInt32})
				}
				for i := 0; i < 30; i++ {
					in = append(in, NodeRank{Rank: -30 + i})
				}
				return in
			}(), 50, 60),
	)

	It("preserves the leading tail slice when cap kicks in", func() {
		// Split-out because the assertion is on the specific tail values,
		// not just the length invariant covered by the table above.
		in := ranks(math.MinInt32, math.MinInt32, math.MinInt32, -5, -4, -3, -2, -1)
		got := capNodeRanks(in, 2)
		Expect(got).To(HaveLen(5))
		Expect(got[3].Rank).To(Equal(-5))
		Expect(got[4].Rank).To(Equal(-4))
	})

	It("preserves the leading tail slice when there is no Group A", func() {
		in := ranks(-5, -4, -3, -2, -1)
		got := capNodeRanks(in, 3)
		Expect(got).To(HaveLen(3))
		Expect(got[0].Rank).To(Equal(-5))
		Expect(got[2].Rank).To(Equal(-3))
	})
})

var _ = Describe("filterNoOpNodes", func() {
	It("drops nodes whose pods already match the planned rank", func() {
		in := []NodeRank{
			{Rank: -5, Pods: []*corev1.Pod{podWithCost("-5"), podWithCost("-5")}},
			{Rank: -4, Pods: []*corev1.Pod{podWithCost("-5"), podWithCost("-3")}},
		}
		got := filterNoOpNodes(in)
		Expect(got).To(HaveLen(1))
		Expect(got[0].Rank).To(Equal(-4))
	})

	It("always admits Group A nodes even when pods already carry the sentinel", func() {
		minStr := strconv.Itoa(math.MinInt32)
		in := []NodeRank{{Rank: math.MinInt32, Pods: []*corev1.Pod{podWithCost(minStr)}}}
		got := filterNoOpNodes(in)
		Expect(got).To(HaveLen(1))
	})

	It("drops Group D nodes whose pods are all already cleared", func() {
		in := []NodeRank{
			{HasDoNotDisrupt: true, Pods: []*corev1.Pod{podWithCost(""), podWithCost("")}},
			{HasDoNotDisrupt: true, Pods: []*corev1.Pod{podWithCost("-5"), podWithCost("")}},
		}
		got := filterNoOpNodes(in)
		Expect(got).To(HaveLen(1))
		Expect(got[0].HasDoNotDisrupt).To(BeTrue())
	})

	It("drops nodes with empty pod lists (unless Group A)", func() {
		in := []NodeRank{
			{Rank: -3, Pods: nil},
			{Rank: math.MinInt32, Pods: nil},
		}
		got := filterNoOpNodes(in)
		Expect(got).To(HaveLen(1))
		Expect(got[0].Rank).To(Equal(int(math.MinInt32)))
	})
})

var _ = Describe("livePodCount", func() {
	It("counts only non-terminating pods", func() {
		// Node A has 5 pods, 3 already terminating: live count = 2.
		// Node B has 3 live pods only: live count = 3.
		// sortByPodCount should place A before B despite the raw count 5 > 3.
		pods := map[string][]*corev1.Pod{
			"a": {livePod(), livePod(), deletingPod(), deletingPod(), deletingPod()},
			"b": {livePod(), livePod(), livePod()},
		}
		Expect(livePodCount(pods, "a")).To(Equal(2))
		Expect(livePodCount(pods, "b")).To(Equal(3))
	})

	It("maps missing entries to math.MaxInt so lookup failures sort last", func() {
		pods := map[string][]*corev1.Pod{"a": {livePod()}}
		Expect(livePodCount(pods, "not-present")).To(Equal(math.MaxInt))
	})

	It("returns 0 when every pod on the node is terminating", func() {
		pods := map[string][]*corev1.Pod{
			"draining": {deletingPod(), deletingPod(), deletingPod()},
		}
		Expect(livePodCount(pods, "draining")).To(Equal(0))
	})
})
