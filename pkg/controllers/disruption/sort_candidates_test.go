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

func makeCandidate(drifted bool, driftTime time.Time, cost float64) *Candidate {
	nc := &v1.NodeClaim{}
	if drifted {
		nc.Status.Conditions = []status.Condition{
			{
				Type:               v1.ConditionTypeDrifted,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(driftTime),
			},
		}
	}
	return &Candidate{
		StateNode:      &state.StateNode{NodeClaim: nc},
		DisruptionCost: cost,
	}
}

func TestSortCandidates_DriftedBeforeNonDrifted(t *testing.T) {
	c := consolidation{}
	now := time.Now()
	candidates := []*Candidate{
		makeCandidate(false, time.Time{}, 1.0),
		makeCandidate(true, now, 2.0),
		makeCandidate(false, time.Time{}, 0.5),
	}
	result := c.sortCandidates(candidates)
	if !result[0].StateNode.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue() {
		t.Fatal("expected drifted candidate first")
	}
	for _, r := range result[1:] {
		if r.StateNode.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue() {
			t.Fatal("expected non-drifted candidates after drifted")
		}
	}
}

func TestSortCandidates_OldestDriftedFirst(t *testing.T) {
	c := consolidation{}
	now := time.Now()
	candidates := []*Candidate{
		makeCandidate(true, now, 1.0),
		makeCandidate(true, now.Add(-1*time.Hour), 2.0),
		makeCandidate(true, now.Add(-2*time.Hour), 3.0),
	}
	result := c.sortCandidates(candidates)
	for i := 0; i < len(result)-1; i++ {
		iTime := result[i].StateNode.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).LastTransitionTime
		jTime := result[i+1].StateNode.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).LastTransitionTime
		if !iTime.Before(&jTime) && !iTime.Equal(&jTime) {
			t.Fatalf("expected oldest drifted first at index %d", i)
		}
	}
}

func TestSortCandidates_NonDriftedOrderUnchanged(t *testing.T) {
	c := consolidation{}
	candidates := []*Candidate{
		makeCandidate(false, time.Time{}, 3.0),
		makeCandidate(false, time.Time{}, 1.0),
		makeCandidate(false, time.Time{}, 2.0),
	}
	result := c.sortCandidates(candidates)
	for i := 0; i < len(result)-1; i++ {
		if result[i].DisruptionCost > result[i+1].DisruptionCost {
			t.Fatalf("expected non-drifted sorted by disruption cost, got %f > %f at index %d",
				result[i].DisruptionCost, result[i+1].DisruptionCost, i)
		}
	}
}
