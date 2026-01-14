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

func TestGetCommandEstimatedSavings(t *testing.T) {
	// This is a basic test to ensure the function doesn't panic
	// Full testing would require setting up candidates with instance types
	cmd := Command{
		Candidates:   []*Candidate{},
		Replacements: []*Replacement{},
	}
	
	savings := getCommandEstimatedSavings(cmd)
	if savings < 0 {
		t.Errorf("getCommandEstimatedSavings() returned negative value: %v", savings)
	}
}
