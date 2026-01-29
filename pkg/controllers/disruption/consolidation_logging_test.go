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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/test"
)

const (
	// floatComparisonDelta is the tolerance for floating point comparisons in tests
	floatComparisonDelta = 0.001
)

func TestGetValidationFailureReason(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: "unknown",
		},
		{
			name:     "non-validation error",
			err:      errors.New("some other error"),
			expected: "unknown",
		},
		{
			name:     "budget error - nominated",
			err:      NewBudgetValidationError(errors.New("a candidate was nominated during validation")),
			expected: "budget",
		},
		{
			name:     "budget error - budget",
			err:      NewBudgetValidationError(errors.New("can no longer be disrupted without violating budgets")),
			expected: "budget",
		},
		{
			name:     "scheduling error",
			err:      NewSchedulingValidationError(errors.New("scheduling simulation produced new results")),
			expected: "scheduling",
		},
		{
			name:     "churn error - no longer valid",
			err:      NewChurnValidationError(errors.New("candidates are no longer valid")),
			expected: "churn",
		},
		{
			name:     "churn error - churn",
			err:      NewChurnValidationError(errors.New("pod churn detected")),
			expected: "churn",
		},
		{
			name:     "generic validation error",
			err:      NewValidationError(errors.New("something unexpected")),
			expected: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getValidationFailureReason(tt.err)
			if result != tt.expected {
				t.Errorf("getValidationFailureReason() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestGetCommandEstimatedSavings_MultipleReplacements(t *testing.T) {
	// This test verifies that EstimatedSavings correctly sums costs from multiple
	// replacement NodeClaims. While current consolidation is 1->1, this future-proofs
	// for potential N->M consolidation scenarios.

	// Create mock candidates with pricing
	candidate1 := mockCandidate("node-1")
	candidate1.instanceType = &cloudprovider.InstanceType{
		Name: "source-type",
		Offerings: cloudprovider.Offerings{
			{Price: 0.50},
		},
	}

	cmd := Command{
		Candidates:   []*Candidate{candidate1},
		Replacements: []*Replacement{{}, {}}, // 2 replacements
		Results: scheduling.Results{
			NewNodeClaims: []*scheduling.NodeClaim{
				{
					NodeClaimTemplate: scheduling.NodeClaimTemplate{
						InstanceTypeOptions: []*cloudprovider.InstanceType{
							{
								Name: "dest-type-1",
								Offerings: cloudprovider.Offerings{
									{Price: 0.20},
								},
							},
						},
					},
				},
				{
					NodeClaimTemplate: scheduling.NodeClaimTemplate{
						InstanceTypeOptions: []*cloudprovider.InstanceType{
							{
								Name: "dest-type-2",
								Offerings: cloudprovider.Offerings{
									{Price: 0.15},
								},
							},
						},
					},
				},
			},
		},
	}

	// Expected: sourcePrice (0.50) - (destPrice1 (0.20) + destPrice2 (0.15)) = 0.15
	savings := cmd.EstimatedSavings()
	expectedSavings := 0.15

	// Use tolerance for floating point comparison
	if diff := savings - expectedSavings; diff < -floatComparisonDelta || diff > floatComparisonDelta {
		t.Errorf("Command.EstimatedSavings() = %v, want %v (verifying multi-NodeClaim cost summing)", savings, expectedSavings)
	}
}

func TestGetCommandEstimatedSavings_EdgeCases(t *testing.T) {
	tests := []struct {
		name            string
		cmd             Command
		expectedSavings float64
	}{
		{
			name: "empty NodeClaim list",
			cmd: Command{
				Replacements: []*Replacement{{}},
			},
			expectedSavings: 0.0,
		},
		{
			name: "NodeClaim with no InstanceTypeOptions",
			cmd: Command{
				Replacements: []*Replacement{{}},
				Results: scheduling.Results{
					NewNodeClaims: []*scheduling.NodeClaim{
						{
							NodeClaimTemplate: scheduling.NodeClaimTemplate{
								InstanceTypeOptions: []*cloudprovider.InstanceType{},
							},
						},
					},
				},
			},
			expectedSavings: 0.0,
		},
		{
			name: "NodeClaim with empty Offerings",
			cmd: Command{
				Replacements: []*Replacement{{}},
				Results: scheduling.Results{
					NewNodeClaims: []*scheduling.NodeClaim{
						{
							NodeClaimTemplate: scheduling.NodeClaimTemplate{
								InstanceTypeOptions: []*cloudprovider.InstanceType{
									{
										Name:      "instance-type",
										Offerings: cloudprovider.Offerings{},
									},
								},
							},
						},
					},
				},
			},
			expectedSavings: 0.0,
		},
		{
			name: "delete consolidation - no replacements",
			cmd: Command{
				Replacements: []*Replacement{},
			},
			expectedSavings: 0.0, // No candidates, so sourcePrice = 0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			savings := tt.cmd.EstimatedSavings()
			if diff := savings - tt.expectedSavings; diff < -floatComparisonDelta || diff > floatComparisonDelta {
				t.Errorf("Command.EstimatedSavings() = %v, want %v", savings, tt.expectedSavings)
			}
		})
	}
}

func TestCommand_String(t *testing.T) {
	tests := []struct {
		name     string
		cmd      Command
		expected string
	}{
		{
			name: "delete single node",
			cmd: Command{
				Candidates:   []*Candidate{mockCandidate("node-1")},
				Replacements: []*Replacement{},
			},
			expected: "delete: [node-1]",
		},
		{
			name: "delete multiple nodes",
			cmd: Command{
				Candidates:   []*Candidate{mockCandidate("node-1"), mockCandidate("node-2")},
				Replacements: []*Replacement{},
			},
			expected: "delete: [node-1, node-2]",
		},
		{
			name: "replace with single replacement",
			cmd: Command{
				Candidates:   []*Candidate{mockCandidate("node-1")},
				Replacements: []*Replacement{{}},
			},
			expected: "replace: [node-1] -> [1 replacement]",
		},
		{
			name: "replace with multiple replacements",
			cmd: Command{
				Candidates:   []*Candidate{mockCandidate("node-1")},
				Replacements: []*Replacement{{}, {}},
			},
			expected: "replace: [node-1] -> [2 replacements]",
		},
		{
			name: "empty command",
			cmd: Command{
				Candidates:   []*Candidate{},
				Replacements: []*Replacement{},
			},
			expected: "no-op: []",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.cmd.String()
			if result != tt.expected {
				t.Errorf("Command.String() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func mockCandidate(name string) *Candidate {
	return &Candidate{
		StateNode: &state.StateNode{
			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
			},
		},
	}
}

func TestConsolidationCandidateEvent(t *testing.T) {
	recorder := test.NewEventRecorder()

	// Create a mock candidate with Node and NodeClaim
	candidate := &Candidate{
		StateNode: &state.StateNode{
			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					UID:  "test-node-uid",
				},
			},
			NodeClaim: &v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-nodeclaim",
					UID:  "test-nodeclaim-uid",
				},
			},
		},
	}

	// Create a command with candidates but no replacements (delete consolidation)
	cmd := Command{
		Candidates:   []*Candidate{candidate},
		Replacements: []*Replacement{}, // Empty for delete consolidation
	}

	// Publish the event
	recorder.Publish(disruptionevents.ConsolidationCandidate(
		candidate.Node,
		candidate.NodeClaim,
		cmd.String(),
		0.0, // Use fixed savings to avoid EstimatedSavings calculation
	)...)

	// Verify the event was published
	if recorder.Calls(events.ConsolidationCandidate) != 2 {
		t.Errorf("Expected 2 ConsolidationCandidate events (Node + NodeClaim), got %d",
			recorder.Calls(events.ConsolidationCandidate))
	}
}
