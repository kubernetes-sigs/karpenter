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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Internal-package specs for helpers that need access to unexported symbols
// (RankForBC, nodeMutatesAnyPod, lo.GroupBy sort-preservation contract).
// The dot import shares Ginkgo's global spec registry with suite_test.go so
// these run under the same RunSpecs entrypoint.
var _ = Describe("RankForBC", func() {
	It("returns -n for index 0 and -1 for the last index", func() {
		Expect(RankForBC(0, 5)).To(Equal(-5))
		Expect(RankForBC(4, 5)).To(Equal(-1))
	})
	It("produces contiguous negative ranks", func() {
		for n := 1; n <= 10; n++ {
			seen := map[int]struct{}{}
			for i := 0; i < n; i++ {
				r := RankForBC(i, n)
				Expect(r).To(BeNumerically("<", 0), "rank should be negative")
				Expect(r).To(BeNumerically(">=", -n), "rank floor is -n")
				seen[r] = struct{}{}
			}
			Expect(seen).To(HaveLen(n), "all ranks distinct across the slice")
		}
	})
})

var _ = Describe("nodeMutatesAnyPod", func() {
	pod := func(value string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{}}
		if value != "" {
			p.Annotations = map[string]string{corev1.PodDeletionCost: value}
		}
		return p
	}
	It("reports mutation when any pod's annotation differs from the planned rank", func() {
		pods := []*corev1.Pod{pod("-5"), pod("-3")}
		Expect(nodeMutatesAnyPod(pods, -5, false)).To(BeTrue())
	})
	It("reports no-op when every pod already carries the planned rank", func() {
		pods := []*corev1.Pod{pod("-5"), pod("-5")}
		Expect(nodeMutatesAnyPod(pods, -5, false)).To(BeFalse())
	})
	It("reports no-op when every pod's annotation is already cleared", func() {
		pods := []*corev1.Pod{pod(""), pod("")}
		Expect(nodeMutatesAnyPod(pods, 0, true)).To(BeFalse())
	})
	It("reports mutation for cleanup when any pod still carries an annotation", func() {
		pods := []*corev1.Pod{pod("-2"), pod("")}
		Expect(nodeMutatesAnyPod(pods, 0, true)).To(BeTrue())
	})
	It("reports no-op for an empty pod list", func() {
		Expect(nodeMutatesAnyPod(nil, -1, false)).To(BeFalse())
		Expect(nodeMutatesAnyPod(nil, 0, true)).To(BeFalse())
	})
	It("reports mutation when any Group A pod lacks the sentinel", func() {
		sentinel := strconv.Itoa(math.MinInt32)
		pods := []*corev1.Pod{pod(sentinel), pod("")}
		Expect(nodeMutatesAnyPod(pods, math.MinInt32, false)).To(BeTrue())
	})
})

// lo.GroupBy contract: iterates the input slice in order and appends to
// each group. RankNodes relies on this so drift/normal SavingsRatio sort
// runs on partitions that preserve upstream sort order (e.g. name-sorted
// cluster snapshot). If lo ever changes to a map-based implementation
// this test fails loudly and RankNodes needs a stable-sort-then-partition
// fallback.
var _ = Describe("lo.GroupBy sort preservation", func() {
	type item struct {
		id    int
		group string
	}
	It("preserves source order within each group across arbitrary partitions", func() {
		input := []item{
			{id: 0, group: "A"},
			{id: 1, group: "B"},
			{id: 2, group: "A"},
			{id: 3, group: "C"},
			{id: 4, group: "B"},
			{id: 5, group: "A"},
			{id: 6, group: "C"},
			{id: 7, group: "B"},
			{id: 8, group: "A"},
			{id: 9, group: "C"},
		}
		groups := lo.GroupBy(input, func(it item) string { return it.group })
		Expect(groups).To(HaveKey("A"))
		Expect(groups).To(HaveKey("B"))
		Expect(groups).To(HaveKey("C"))
		ids := func(xs []item) []int { return lo.Map(xs, func(x item, _ int) int { return x.id }) }
		Expect(ids(groups["A"])).To(Equal([]int{0, 2, 5, 8}))
		Expect(ids(groups["B"])).To(Equal([]int{1, 4, 7}))
		Expect(ids(groups["C"])).To(Equal([]int{3, 6, 9}))
	})
	It("preserves order for a single-group input", func() {
		input := []int{5, 3, 8, 1, 9, 2, 7, 4, 6}
		groups := lo.GroupBy(input, func(int) string { return "same" })
		Expect(groups["same"]).To(Equal(input))
	})
	It("preserves order across a 100-item interleaved input", func() {
		input := make([]int, 100)
		for i := range input {
			input[i] = i
		}
		groups := lo.GroupBy(input, func(v int) int { return v % 3 })
		for k, want := range map[int][]int{
			0: nil, 1: nil, 2: nil,
		} {
			for i := k; i < 100; i += 3 {
				want = append(want, i)
			}
			Expect(groups[k]).To(Equal(want), "group %d should preserve source order", k)
		}
	})
})
