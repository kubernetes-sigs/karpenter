
package disruption

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

func TestLogValues_PodCountAccumulation(t *testing.T) {
	tests := []struct {
		name              string
		candidates        []*Candidate
		expectedPodCount  int
	}{
		{
			name: "multiple candidates with pods",
			candidates: []*Candidate{
				{
					StateNode: &state.StateNode{
						Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
						NodeClaim: &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}},
					},
					reschedulablePods: []*corev1.Pod{{}, {}}, // 2 pods
				},
				{
					StateNode: &state.StateNode{
						Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}},
						NodeClaim: &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-2"}},
					},
					reschedulablePods: []*corev1.Pod{{}, {}, {}}, // 3 pods
				},
				{
					StateNode: &state.StateNode{
						Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-3"}},
						NodeClaim: &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-3"}},
					},
					reschedulablePods: []*corev1.Pod{{}}, // 1 pod
				},
			},
			expectedPodCount: 6,
		},
		{
			name: "no candidates",
			candidates: []*Candidate{},
			expectedPodCount: 0,
		},
		{
			name: "candidates with no pods",
			candidates: []*Candidate{
				{
					StateNode: &state.StateNode{
						Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
						NodeClaim: &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}},
					},
					reschedulablePods: []*corev1.Pod{},
				},
			},
			expectedPodCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := Command{Candidates: tt.candidates}
			values := cmd.LogValues()
			
			var podCount int
			found := false
			for i := 0; i < len(values); i += 2 {
				if values[i] == "pod-count" {
					podCount = values[i+1].(int)
					found = true
					break
				}
			}
			
			if !found {
				t.Fatal("pod-count not found in LogValues")
			}
			
			if podCount != tt.expectedPodCount {
				t.Errorf("expected pod-count %d, got %d", tt.expectedPodCount, podCount)
			}
		})
	}
}
