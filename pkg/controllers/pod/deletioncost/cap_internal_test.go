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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ranks(pattern ...int) []NodeRank {
	out := make([]NodeRank, 0, len(pattern))
	for _, r := range pattern {
		out = append(out, NodeRank{Rank: r})
	}
	return out
}

func TestCapNodeRanks_ReturnsAllGroupA_CapsTail(t *testing.T) {
	// 3 Group A nodes plus 5 tail nodes; cap=2 → all 3 A kept, tail cut to 2.
	in := ranks(math.MinInt32, math.MinInt32, math.MinInt32, -5, -4, -3, -2, -1)
	got := capNodeRanks(in, 2)
	if len(got) != 5 {
		t.Fatalf("expected 5 nodes retained (3 A + 2 tail), got %d", len(got))
	}
	for i := 0; i < 3; i++ {
		if got[i].Rank != math.MinInt32 {
			t.Errorf("position %d: expected math.MinInt32, got %d", i, got[i].Rank)
		}
	}
	if got[3].Rank != -5 || got[4].Rank != -4 {
		t.Errorf("expected first two tail nodes -5, -4; got %d, %d", got[3].Rank, got[4].Rank)
	}
}

func TestCapNodeRanks_UnboundedGroupA(t *testing.T) {
	// 100 Group A nodes; cap=50 must NOT truncate any of them.
	in := make([]NodeRank, 100)
	for i := range in {
		in[i] = NodeRank{Rank: math.MinInt32}
	}
	got := capNodeRanks(in, 50)
	if len(got) != 100 {
		t.Fatalf("expected all 100 Group A nodes retained, got %d", len(got))
	}
}

func TestCapNodeRanks_AllGroupA_PlusMoreTail(t *testing.T) {
	// 100 Group A + 100 tail; cap=50 → 150 total: 100 A + 50 tail.
	in := make([]NodeRank, 0, 200)
	for i := 0; i < 100; i++ {
		in = append(in, NodeRank{Rank: math.MinInt32})
	}
	for i := 0; i < 100; i++ {
		in = append(in, NodeRank{Rank: -100 + i})
	}
	got := capNodeRanks(in, 50)
	if len(got) != 150 {
		t.Fatalf("expected 150 retained (100 A + 50 tail), got %d", len(got))
	}
	for i := 0; i < 100; i++ {
		if got[i].Rank != math.MinInt32 {
			t.Errorf("position %d: expected Group A sentinel, got %d", i, got[i].Rank)
		}
	}
}

func TestCapNodeRanks_TailUnderCap_Unchanged(t *testing.T) {
	in := ranks(math.MinInt32, -3, -2, -1)
	got := capNodeRanks(in, 50)
	if len(got) != len(in) {
		t.Fatalf("tail below cap should return all input; expected %d, got %d", len(in), len(got))
	}
}

func TestCapNodeRanks_EmptyInput(t *testing.T) {
	got := capNodeRanks(nil, 10)
	if len(got) != 0 {
		t.Fatalf("expected 0 for empty input, got %d", len(got))
	}
}

func TestCapNodeRanks_NoGroupA(t *testing.T) {
	// All nodes in Groups B/C/D; cap applies from the start.
	in := ranks(-5, -4, -3, -2, -1)
	got := capNodeRanks(in, 3)
	if len(got) != 3 {
		t.Fatalf("expected tail cap to 3, got %d", len(got))
	}
	if got[0].Rank != -5 || got[2].Rank != -3 {
		t.Errorf("unexpected tail slice: %+v", got)
	}
}

func TestCapNodeRanks_MixedExactCapBoundary(t *testing.T) {
	// 30 Group A + 30 tail with cap=50: tail below cap so all 60 retained.
	in := make([]NodeRank, 0, 60)
	for i := 0; i < 30; i++ {
		in = append(in, NodeRank{Rank: math.MinInt32})
	}
	for i := 0; i < 30; i++ {
		in = append(in, NodeRank{Rank: -30 + i})
	}
	got := capNodeRanks(in, 50)
	if len(got) != 60 {
		t.Fatalf("expected 60 retained (tail under cap), got %d", len(got))
	}
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

func TestFilterNoOpNodes_DropsRankMatchingNodes(t *testing.T) {
	// Node with rank -5 and all pods already at -5: should be dropped.
	// Node with rank -4 and one pod at -3: should be kept.
	in := []NodeRank{
		{Rank: -5, Pods: []*corev1.Pod{podWithCost("-5"), podWithCost("-5")}},
		{Rank: -4, Pods: []*corev1.Pod{podWithCost("-5"), podWithCost("-3")}},
	}
	got := filterNoOpNodes(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 non-op node retained, got %d", len(got))
	}
	if got[0].Rank != -4 {
		t.Errorf("expected mutating node with rank -4, got %d", got[0].Rank)
	}
}

func TestFilterNoOpNodes_AlwaysAdmitsGroupA(t *testing.T) {
	// Group A node with pods already at math.MinInt32: still admitted.
	minStr := strconv.Itoa(math.MinInt32)
	in := []NodeRank{
		{Rank: math.MinInt32, Pods: []*corev1.Pod{podWithCost(minStr)}},
	}
	got := filterNoOpNodes(in)
	if len(got) != 1 {
		t.Fatalf("Group A node must always be admitted; got %d retained", len(got))
	}
}

func TestFilterNoOpNodes_GroupDDropsWhenAllCleared(t *testing.T) {
	// Group D with no pods carrying the annotation: no-op, drop.
	// Group D with any pod still annotated: keep for the clear.
	in := []NodeRank{
		{HasDoNotDisrupt: true, Pods: []*corev1.Pod{podWithCost(""), podWithCost("")}},
		{HasDoNotDisrupt: true, Pods: []*corev1.Pod{podWithCost("-5"), podWithCost("")}},
	}
	got := filterNoOpNodes(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 Group D node retained (still-annotated pod), got %d", len(got))
	}
	if !got[0].HasDoNotDisrupt {
		t.Errorf("expected retained node to be Group D")
	}
}

func TestFilterNoOpNodes_EmptyPodListIsNoOp(t *testing.T) {
	// A node with zero pods can never mutate anything: dropped (unless A).
	in := []NodeRank{
		{Rank: -3, Pods: nil},
		{Rank: math.MinInt32, Pods: nil}, // Group A still admitted
	}
	got := filterNoOpNodes(in)
	if len(got) != 1 || got[0].Rank != math.MinInt32 {
		t.Fatalf("expected only the Group A entry, got %+v", got)
	}
}
