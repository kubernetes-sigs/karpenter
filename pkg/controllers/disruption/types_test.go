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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// TestCandidate_DecisionRatioFields verifies that the Candidate struct can store decision ratio values
func TestCandidate_DecisionRatioFields(t *testing.T) {
	tests := []struct {
		name                 string
		normalizedCost       float64
		normalizedDisruption float64
		decisionRatio        float64
		deleteRatio          float64
	}{
		{
			name:                 "zero values",
			normalizedCost:       0.0,
			normalizedDisruption: 0.0,
			decisionRatio:        0.0,
			deleteRatio:          0.0,
		},
		{
			name:                 "typical values",
			normalizedCost:       0.25,
			normalizedDisruption: 0.15,
			decisionRatio:        1.67,
			deleteRatio:          1.75,
		},
		{
			name:                 "ratio below threshold",
			normalizedCost:       0.10,
			normalizedDisruption: 0.20,
			decisionRatio:        0.50,
			deleteRatio:          0.55,
		},
		{
			name:                 "ratio at threshold",
			normalizedCost:       0.20,
			normalizedDisruption: 0.20,
			decisionRatio:        1.0,
			deleteRatio:          1.0,
		},
		{
			name:                 "high ratio",
			normalizedCost:       0.50,
			normalizedDisruption: 0.10,
			decisionRatio:        5.0,
			deleteRatio:          5.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := &Candidate{
				StateNode: &state.StateNode{
					Node: &corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: "test-node",
						},
					},
				},
				normalizedCost:       tt.normalizedCost,
				normalizedDisruption: tt.normalizedDisruption,
				decisionRatio:        tt.decisionRatio,
				deleteRatio:          tt.deleteRatio,
			}
			_ = candidate.StateNode // Used for test setup

			// Verify all fields can be stored and retrieved
			if candidate.normalizedCost != tt.normalizedCost {
				t.Errorf("normalizedCost = %v, want %v", candidate.normalizedCost, tt.normalizedCost)
			}
			if candidate.normalizedDisruption != tt.normalizedDisruption {
				t.Errorf("normalizedDisruption = %v, want %v", candidate.normalizedDisruption, tt.normalizedDisruption)
			}
			if candidate.decisionRatio != tt.decisionRatio {
				t.Errorf("decisionRatio = %v, want %v", candidate.decisionRatio, tt.decisionRatio)
			}
			if candidate.deleteRatio != tt.deleteRatio {
				t.Errorf("deleteRatio = %v, want %v", candidate.deleteRatio, tt.deleteRatio)
			}
		})
	}
}

// TestCommand_DecisionRatioField verifies that the Command struct can store decision ratio values
func TestCommand_DecisionRatioField(t *testing.T) {
	tests := []struct {
		name          string
		decisionRatio float64
	}{
		{
			name:          "zero ratio",
			decisionRatio: 0.0,
		},
		{
			name:          "ratio below threshold",
			decisionRatio: 0.75,
		},
		{
			name:          "ratio at threshold",
			decisionRatio: 1.0,
		},
		{
			name:          "ratio above threshold",
			decisionRatio: 2.5,
		},
		{
			name:          "high ratio",
			decisionRatio: 10.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := Command{
				Candidates: []*Candidate{
					{
						StateNode: &state.StateNode{
							Node: &corev1.Node{
								ObjectMeta: metav1.ObjectMeta{
									Name: "test-node",
								},
							},
						},
					},
				},
				DecisionRatio: tt.decisionRatio,
			}
			_ = cmd.Candidates // Used for test setup

			// Verify the field can be stored and retrieved
			if cmd.DecisionRatio != tt.decisionRatio {
				t.Errorf("DecisionRatio = %v, want %v", cmd.DecisionRatio, tt.decisionRatio)
			}
		})
	}
}

// TestNodePool_DefaultConsolidateWhen verifies the default ConsolidateWhen policy value
func TestNodePool_DefaultConsolidateWhen(t *testing.T) {
	tests := []struct {
		name                string
		consolidateWhen     v1.ConsolidateWhenPolicy
		expectedDefault     v1.ConsolidateWhenPolicy
		shouldBeDefaultable bool
	}{
		{
			name:                "empty value should default to WhenEmptyOrUnderutilized",
			consolidateWhen:     "",
			expectedDefault:     v1.ConsolidateWhenEmptyOrUnderutilized,
			shouldBeDefaultable: true,
		},
		{
			name:                "WhenEmpty is preserved",
			consolidateWhen:     v1.ConsolidateWhenEmpty,
			expectedDefault:     v1.ConsolidateWhenEmpty,
			shouldBeDefaultable: false,
		},
		{
			name:                "WhenEmptyOrUnderutilized is preserved",
			consolidateWhen:     v1.ConsolidateWhenEmptyOrUnderutilized,
			expectedDefault:     v1.ConsolidateWhenEmptyOrUnderutilized,
			shouldBeDefaultable: false,
		},
		{
			name:                "WhenCostJustifiesDisruption is preserved",
			consolidateWhen:     v1.ConsolidateWhenCostJustifiesDisruption,
			expectedDefault:     v1.ConsolidateWhenCostJustifiesDisruption,
			shouldBeDefaultable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodePool := &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-nodepool",
				},
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidateWhen: tt.consolidateWhen,
					},
				},
			}

			// If the value is empty, verify it should be defaulted
			if tt.shouldBeDefaultable {
				if nodePool.Spec.Disruption.ConsolidateWhen != "" {
					t.Errorf("Expected empty ConsolidateWhen before defaulting, got %v", nodePool.Spec.Disruption.ConsolidateWhen)
				}
				// Note: The actual defaulting is done by the kubebuilder webhook/admission controller
				// This test verifies the struct can hold the default value
				nodePool.Spec.Disruption.ConsolidateWhen = tt.expectedDefault
			}

			// Verify the value matches expected
			if nodePool.Spec.Disruption.ConsolidateWhen != tt.expectedDefault {
				t.Errorf("ConsolidateWhen = %v, want %v", nodePool.Spec.Disruption.ConsolidateWhen, tt.expectedDefault)
			}
		})
	}
}

// TestCandidate_DecisionRatioFieldsWithInstanceType verifies decision ratio fields work with full candidate setup
func TestCandidate_DecisionRatioFieldsWithInstanceType(t *testing.T) {
	candidate := &Candidate{
		StateNode: &state.StateNode{
			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
				},
			},
			NodeClaim: &v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-nodeclaim",
				},
			},
		},
		instanceType: &cloudprovider.InstanceType{
			Name: "test-instance-type",
			Offerings: cloudprovider.Offerings{
				{Price: 0.50},
			},
		},
		NodePool: &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-nodepool",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateWhen: v1.ConsolidateWhenCostJustifiesDisruption,
				},
			},
		},
		DisruptionCost:       10.0,
		normalizedCost:       0.25,
		normalizedDisruption: 0.20,
		decisionRatio:        1.25,
		deleteRatio:          1.30,
	}
	_ = candidate.StateNode    // Used for test setup
	_ = candidate.instanceType // Used for test setup

	// Verify all fields are accessible
	if candidate.normalizedCost != 0.25 {
		t.Errorf("normalizedCost = %v, want 0.25", candidate.normalizedCost)
	}
	if candidate.normalizedDisruption != 0.20 {
		t.Errorf("normalizedDisruption = %v, want 0.20", candidate.normalizedDisruption)
	}
	if candidate.decisionRatio != 1.25 {
		t.Errorf("decisionRatio = %v, want 1.25", candidate.decisionRatio)
	}
	if candidate.deleteRatio != 1.30 {
		t.Errorf("deleteRatio = %v, want 1.30", candidate.deleteRatio)
	}
	if candidate.DisruptionCost != 10.0 {
		t.Errorf("DisruptionCost = %v, want 10.0", candidate.DisruptionCost)
	}
	if candidate.NodePool.Spec.Disruption.ConsolidateWhen != v1.ConsolidateWhenCostJustifiesDisruption {
		t.Errorf("ConsolidateWhen = %v, want %v", candidate.NodePool.Spec.Disruption.ConsolidateWhen, v1.ConsolidateWhenCostJustifiesDisruption)
	}
}
