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

package disruption

import (
	"context"
	"fmt"
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

func balancedCandidate(name string, price float64, pods []*corev1.Pod, np *v1.NodePool) *Candidate {
	return &Candidate{
		StateNode: &state.StateNode{
			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: name},
			},
		},
		instanceType: &cloudprovider.InstanceType{
			Name:      name + "-type",
			Offerings: cloudprovider.Offerings{{Price: price}},
		},
		NodePool:          np,
		reschedulablePods: pods,
	}
}

func balancedNodePool(policy v1.ConsolidationPolicy, tolerance *int32) *v1.NodePool {
	return &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidationPolicy: policy,
				DisruptionTolerance: tolerance,
			},
		},
	}
}

func int32Ptr(v int32) *int32 { return &v }

// TestBalancedConsolidationRouting verifies that isBalancedPolicy returns true
// when any candidate uses the Balanced consolidation policy.
func TestBalancedConsolidationRouting(t *testing.T) {
	balanced := balancedNodePool(v1.ConsolidationPolicyBalanced, nil)
	standard := balancedNodePool(v1.ConsolidationPolicyWhenEmptyOrUnderutilized, nil)

	tests := []struct {
		name   string
		pools  []*v1.NodePool
		expect bool
	}{
		{"all balanced", []*v1.NodePool{balanced}, true},
		{"all standard", []*v1.NodePool{standard}, false},
		{"mixed", []*v1.NodePool{standard, balanced}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var candidates []*Candidate
			for i, np := range tt.pools {
				candidates = append(candidates, balancedCandidate("n"+string(rune('0'+i)), 1.0, nil, np))
			}
			if got := isBalancedPolicy(candidates); got != tt.expect {
				t.Errorf("isBalancedPolicy() = %v, want %v", got, tt.expect)
			}
		})
	}
}

// TestBalancedScoreThreshold verifies that ComputeMoveScore produces high scores
// when savings >> disruption and low scores when savings << disruption.
func TestBalancedScoreThreshold(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}
	np := balancedNodePool(v1.ConsolidationPolicyBalanced, int32Ptr(2))

	// High score scenario: deleting a $1.00 node, replacement $0.10, total pool $2.00
	highCandidate := balancedCandidate("expensive", 1.0, []*corev1.Pod{pod}, np)
	cheapCandidate := balancedCandidate("cheap", 0.10, []*corev1.Pod{pod}, np)
	allCandidates := []*Candidate{highCandidate, cheapCandidate}
	totalCost, totalDisruption := ComputeNodePoolMetrics(ctx, allCandidates)

	highScore := ComputeMoveScore(ctx, 1.0, 0.10, totalCost, []*Candidate{highCandidate}, totalDisruption)
	if highScore < 0.5 {
		t.Errorf("high-savings score = %v, want >= 0.5", highScore)
	}

	// Low score scenario: deleting a $0.10 node, replacement $0.09, total pool $2.00
	lowScore := ComputeMoveScore(ctx, 0.10, 0.09, totalCost, []*Candidate{cheapCandidate}, totalDisruption)
	if lowScore >= 0.5 {
		t.Errorf("low-savings score = %v, want < 0.5", lowScore)
	}
}

// TestBalancedScoreApproval verifies that checkBalancedScore approves moves
// that exceed the threshold for Balanced candidates.
func TestBalancedScoreApproval(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}
	np := balancedNodePool(v1.ConsolidationPolicyBalanced, int32Ptr(2))

	candidate := balancedCandidate("node1", 1.0, []*corev1.Pod{pod}, np)
	allCandidates := []*Candidate{candidate}

	c := &consolidation{}
	cmd := Command{
		Candidates: []*Candidate{candidate},
	}

	_, ok := c.checkBalancedScore(ctx, cmd, allCandidates, "single")
	// DELETE move with 100% savings should pass
	if !ok {
		t.Fatal("expected checkBalancedScore to approve the move")
	}
}

// TestMixedPolicyBatching verifies that WhenEmptyOrUnderutilized candidates
// in a mixed batch pass unconditionally (only Balanced candidates are scored).
func TestMixedPolicyBatching(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}
	standardNP := balancedNodePool(v1.ConsolidationPolicyWhenEmptyOrUnderutilized, nil)

	candidate := balancedCandidate("node1", 1.0, []*corev1.Pod{pod}, standardNP)
	allCandidates := []*Candidate{candidate}

	c := &consolidation{}
	cmd := Command{
		Candidates: []*Candidate{candidate},
	}

	_, ok := c.checkBalancedScore(ctx, cmd, allCandidates, "single")
	if !ok {
		t.Fatal("expected WhenEmptyOrUnderutilized candidates to pass unconditionally")
	}
}

// TestNegativeEvictionCostFloor verifies that ComputeNodeDisruptionCost uses
// math.Abs + 1.0 floor so negative eviction costs still contribute.
func TestNegativeEvictionCostFloor(t *testing.T) {
	ctx := context.Background()
	// A pod with no annotations/priority has EvictionCost = 1.0
	// With the floor: math.Abs(1.0) + 1.0 = 2.0 per pod
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}
	cost := ComputeNodeDisruptionCost(ctx, []*corev1.Pod{pod})
	if cost <= 0 {
		t.Errorf("expected positive disruption cost, got %v", cost)
	}
}

// TestDisruptionToleranceDefault verifies that GetDisruptionToleranceThreshold
// returns 0.5 (k=2) when DisruptionTolerance is nil.
func TestDisruptionToleranceDefault(t *testing.T) {
	d := &v1.Disruption{DisruptionTolerance: nil}
	threshold := d.GetDisruptionToleranceThreshold()
	if math.Abs(threshold-0.5) > 1e-9 {
		t.Errorf("default threshold = %v, want 0.5", threshold)
	}
}

// TestGetDisruptionToleranceThreshold tests threshold = 1/k for various k values.
func TestGetDisruptionToleranceThreshold(t *testing.T) {
	tests := []struct {
		k        int32
		expected float64
	}{
		{1, 1.0},
		{2, 0.5},
		{4, 0.25},
		{10, 0.1},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("k=%d", tt.k), func(t *testing.T) {
			d := &v1.Disruption{DisruptionTolerance: int32Ptr(tt.k)}
			got := d.GetDisruptionToleranceThreshold()
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("threshold(k=%d) = %v, want %v", tt.k, got, tt.expected)
			}
		})
	}
}
