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
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

func makeNodePool(name string, annotations map[string]string) *v1.NodePool {
	return &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
	}
}

func makeDriftedStateNode(nodePoolName string, driftTime time.Time) *state.StateNode {
	nc := &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePoolName},
		},
	}
	// Directly set conditions to control LastTransitionTime precisely
	nc.SetConditions([]status.Condition{
		{
			Type:               v1.ConditionTypeDrifted,
			Status:             metav1.ConditionTrue,
			Reason:             v1.ConditionTypeDrifted,
			LastTransitionTime: metav1.Time{Time: driftTime},
		},
	})
	return &state.StateNode{NodeClaim: nc}
}

func makeNonDriftedStateNode(nodePoolName string) *state.StateNode {
	nc := &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePoolName},
		},
	}
	return &state.StateNode{NodeClaim: nc}
}

func TestComputeDriftShare_NoDriftedNodes(t *testing.T) {
	np := makeNodePool("test", map[string]string{v1.DriftSLOAnnotationKey: "24h"})
	nodes := state.StateNodes{makeNonDriftedStateNode("test")}
	share := ComputeDriftShare(np, nodes, 10, time.Now())
	if share != 0 {
		t.Errorf("expected drift_share=0 when no nodes drifted, got %d", share)
	}
}

func TestComputeDriftShare_NoAnnotation(t *testing.T) {
	np := makeNodePool("test", nil)
	now := time.Now()
	nodes := state.StateNodes{makeDriftedStateNode("test", now.Add(-1*time.Hour))}
	share := ComputeDriftShare(np, nodes, 10, now)
	if share != 0 {
		t.Errorf("expected drift_share=0 when no annotation, got %d", share)
	}
}

func TestComputeDriftShare_IncreasesAsDeadlineApproaches(t *testing.T) {
	np := makeNodePool("test", map[string]string{v1.DriftSLOAnnotationKey: "1h"})
	driftTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	nodes := make(state.StateNodes, 10)
	for i := range nodes {
		nodes[i] = makeDriftedStateNode("test", driftTime)
	}

	earlyShare := ComputeDriftShare(np, nodes, 10, driftTime)
	midShare := ComputeDriftShare(np, nodes, 10, driftTime.Add(30*time.Minute))
	lateShare := ComputeDriftShare(np, nodes, 10, driftTime.Add(55*time.Minute))

	if midShare < earlyShare {
		t.Errorf("drift_share should increase as deadline approaches: early=%d, mid=%d", earlyShare, midShare)
	}
	if lateShare < midShare {
		t.Errorf("drift_share should increase as deadline approaches: mid=%d, late=%d", midShare, lateShare)
	}
}

func TestComputeDriftShare_PastDeadline(t *testing.T) {
	np := makeNodePool("test", map[string]string{v1.DriftSLOAnnotationKey: "1h"})
	driftTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nodes := state.StateNodes{makeDriftedStateNode("test", driftTime)}

	// 2 hours past drift time, 1h SLO → past deadline
	share := ComputeDriftShare(np, nodes, 10, driftTime.Add(2*time.Hour))
	if share != 10 {
		t.Errorf("expected drift_share=total_budget when past deadline, got %d", share)
	}
}

func TestComputeDriftShare_InvalidAnnotation(t *testing.T) {
	np := makeNodePool("test", map[string]string{v1.DriftSLOAnnotationKey: "not-a-duration"})
	now := time.Now()
	nodes := state.StateNodes{makeDriftedStateNode("test", now.Add(-1*time.Hour))}
	share := ComputeDriftShare(np, nodes, 10, now)
	if share != 0 {
		t.Errorf("expected drift_share=0 for invalid annotation, got %d", share)
	}
}

func TestComputeDriftShare_MultiWaveOldestDrivesDeadline(t *testing.T) {
	np := makeNodePool("test", map[string]string{v1.DriftSLOAnnotationKey: "2h"})
	wave1Time := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wave2Time := wave1Time.Add(1 * time.Hour)

	nodes := state.StateNodes{
		makeDriftedStateNode("test", wave1Time),
		makeDriftedStateNode("test", wave1Time),
		makeDriftedStateNode("test", wave2Time),
	}

	// At wave1Time + 1h30m: deadline for wave1 is wave1Time+2h = 30min remaining
	now := wave1Time.Add(90 * time.Minute)
	share := ComputeDriftShare(np, nodes, 10, now)
	if share < 1 {
		t.Errorf("expected drift_share >= 1 for multi-wave scenario, got %d", share)
	}
}
