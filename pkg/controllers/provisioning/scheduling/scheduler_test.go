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

package scheduling

import (
	"errors"
	"fmt"
	"testing"
)

func TestSortSchedulingErrors(t *testing.T) {
	nodepoolLimitExceeded := fmt.Errorf("all available instance types exceed limits for nodepool")
	nodepoolNodeLimitExhausted := fmt.Errorf("node limits have been exhausted for nodepool")
	resourceExceeded := fmt.Errorf("instance type exceeds resources")
	incompatibleRequirements := fmt.Errorf("incompatible requirements")
	offeringError := fmt.Errorf("no matching offering")
	hostPortError := fmt.Errorf("host port conflict")
	taintError := fmt.Errorf("did not tolerate taint")
	unknownError := fmt.Errorf("something unexpected")
	nilErr := error(nil)

	tests := []struct {
		name     string
		err      error
		wantRank int
	}{
		{"nodepool resource limits exceeded", nodepoolLimitExceeded, 0},
		{"nodepool node limits exhausted", nodepoolNodeLimitExhausted, 0},
		{"instance type exceeds resources", resourceExceeded, 0},
		{"incompatible requirements", incompatibleRequirements, 10},
		{"offering error", offeringError, 20},
		{"host port error", hostPortError, 30},
		{"taint error", taintError, 40},
		{"unknown error", unknownError, 50},
		{"nil error", nilErr, 100},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := schedulingErrorRank(testCase.err)
			if got != testCase.wantRank {
				t.Errorf("schedulingErrorRank(%v) = %d, want %d", testCase.err, got, testCase.wantRank)
			}
		})
	}

	t.Run("sortSchedulingErrors orders by rank", func(t *testing.T) {
		errs := []error{unknownError, taintError, nodepoolLimitExceeded, incompatibleRequirements}
		sortSchedulingErrors(errs)
		for idx := 1; idx < len(errs); idx++ {
			if schedulingErrorRank(errs[idx-1]) > schedulingErrorRank(errs[idx]) {
				t.Errorf("errors not sorted: rank(%v)=%d > rank(%v)=%d",
					errs[idx-1], schedulingErrorRank(errs[idx-1]),
					errs[idx], schedulingErrorRank(errs[idx]))
			}
		}
		if !errors.Is(errs[0], nodepoolLimitExceeded) {
			t.Errorf("expected nodepool limit error first, got %v", errs[0])
		}
	})
}
