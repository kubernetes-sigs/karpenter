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

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
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
	// This test verifies that getCommandEstimatedSavings correctly sums
	// costs from multiple replacement NodeClaims, future-proofing for
	// potential N->M consolidation scenarios.
	//
	// We can't easily test with real Candidates (requires StateNode setup),
	// so we test the destination cost summing logic in isolation.
	//
	// Scenario: 2 replacement nodes with different costs
	// Expected: Both costs should be summed

	cmd := Command{
		Replacements: []*Replacement{{}, {}}, // 2 replacements to avoid delete path
	}
	cmd.Results.NewNodeClaims = []*scheduling.NodeClaim{
		{
			NodeClaimTemplate: scheduling.NodeClaimTemplate{
				InstanceTypeOptions: []*cloudprovider.InstanceType{
					{
						Name: "instance-type-1",
						Offerings: cloudprovider.Offerings{
							{Price: 0.30},
						},
					},
				},
			},
		},
		{
			NodeClaimTemplate: scheduling.NodeClaimTemplate{
				InstanceTypeOptions: []*cloudprovider.InstanceType{
					{
						Name: "instance-type-2",
						Offerings: cloudprovider.Offerings{
							{Price: 0.40},
						},
					},
				},
			},
		},
	}

	// With no candidates (sourcePrice = 0), we're testing the destination summing:
	// savings = 0 - (0.30 + 0.40) = -0.70
	// Negative savings means cost increase, which wouldn't happen in real consolidation,
	// but this test verifies the summing logic works for multiple NodeClaims.
	savings := cmd.EstimatedSavings()
	expectedSavings := -0.70

	if savings != expectedSavings {
		t.Errorf("Command.EstimatedSavings() = %v, want %v (verifying multi-NodeClaim summing)", savings, expectedSavings)
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
			if savings != tt.expectedSavings {
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
