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

package v1

import (
	"context"
	"strings"
	"testing"
)

func int32Ptr(v int32) *int32 { return &v }

// --- Test 2: Validation ---

// TestValidateConsolidationThreshold_RejectsWithoutBalanced verifies that
// RuntimeValidate returns an error when consolidationThreshold is set but
// consolidationPolicy is not Balanced.
func TestValidateConsolidationThreshold_RejectsWithoutBalanced(t *testing.T) {
	np := &NodePool{}
	np.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenEmptyOrUnderutilized
	np.Spec.Disruption.ConsolidationThreshold = int32Ptr(2)

	err := np.RuntimeValidate(context.Background())
	if err == nil {
		t.Fatal("expected error when consolidationThreshold is set with non-Balanced policy")
	}
	if !strings.Contains(err.Error(), "consolidationThreshold is only valid when consolidationPolicy is Balanced") {
		t.Errorf("unexpected error message: %s", err.Error())
	}
}

// TestValidateConsolidationThreshold_RejectsWithWhenEmpty verifies rejection
// when policy is WhenEmpty.
func TestValidateConsolidationThreshold_RejectsWithWhenEmpty(t *testing.T) {
	np := &NodePool{}
	np.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenEmpty
	np.Spec.Disruption.ConsolidationThreshold = int32Ptr(2)

	err := np.RuntimeValidate(context.Background())
	if err == nil {
		t.Fatal("expected error when consolidationThreshold is set with WhenEmpty policy")
	}
}

// TestValidateConsolidationThreshold_PassesWithBalanced verifies that validation
// passes when consolidationThreshold is set with consolidationPolicy: Balanced.
func TestValidateConsolidationThreshold_PassesWithBalanced(t *testing.T) {
	np := &NodePool{}
	np.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyBalanced
	np.Spec.Disruption.ConsolidationThreshold = int32Ptr(2)

	err := np.Spec.Disruption.validateConsolidationThreshold()
	if err != nil {
		t.Errorf("expected no error for Balanced policy with threshold, got: %s", err)
	}
}

// TestValidateConsolidationThreshold_PassesWithNilThreshold verifies that
// validation passes when consolidationThreshold is nil regardless of policy.
func TestValidateConsolidationThreshold_PassesWithNilThreshold(t *testing.T) {
	np := &NodePool{}
	np.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenEmptyOrUnderutilized
	np.Spec.Disruption.ConsolidationThreshold = nil

	err := np.Spec.Disruption.validateConsolidationThreshold()
	if err != nil {
		t.Errorf("expected no error when threshold is nil, got: %s", err)
	}
}

// TestValidateConsolidationThreshold_BoundaryValues tests the valid range [1, 3].
// Note: The range [1, 3] is enforced by kubebuilder CEL validation at the API level.
// The RuntimeValidate only checks the policy association. These tests verify the
// policy check works for each valid value.
func TestValidateConsolidationThreshold_BoundaryValues(t *testing.T) {
	tests := []struct {
		name      string
		threshold int32
		policy    ConsolidationPolicy
		wantErr   bool
	}{
		{"threshold 1 with Balanced", 1, ConsolidationPolicyBalanced, false},
		{"threshold 2 with Balanced", 2, ConsolidationPolicyBalanced, false},
		{"threshold 3 with Balanced", 3, ConsolidationPolicyBalanced, false},
		{"threshold 1 with WhenEmptyOrUnderutilized", 1, ConsolidationPolicyWhenEmptyOrUnderutilized, true},
		{"threshold 3 with WhenEmptyOrUnderutilized", 3, ConsolidationPolicyWhenEmptyOrUnderutilized, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &Disruption{
				ConsolidationPolicy:    tc.policy,
				ConsolidationThreshold: int32Ptr(tc.threshold),
			}
			err := d.validateConsolidationThreshold()
			if tc.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got: %s", err)
			}
		})
	}
}

// --- Test 3: Defaulting ---

// TestSetDefaults_BalancedPolicyDefaultsThresholdTo2 verifies that SetDefaults
// sets consolidationThreshold to 2 when consolidationPolicy is Balanced and
// threshold is nil.
func TestSetDefaults_BalancedPolicyDefaultsThresholdTo2(t *testing.T) {
	np := &NodePool{}
	np.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyBalanced
	np.Spec.Disruption.ConsolidationThreshold = nil

	np.SetDefaults(context.Background())

	if np.Spec.Disruption.ConsolidationThreshold == nil {
		t.Fatal("expected consolidationThreshold to be set, got nil")
	}
	if *np.Spec.Disruption.ConsolidationThreshold != DefaultConsolidationThreshold {
		t.Errorf("expected threshold %d, got %d", DefaultConsolidationThreshold, *np.Spec.Disruption.ConsolidationThreshold)
	}
	if *np.Spec.Disruption.ConsolidationThreshold != 2 {
		t.Errorf("expected threshold 2, got %d", *np.Spec.Disruption.ConsolidationThreshold)
	}
}

// TestSetDefaults_NonBalancedPolicyDoesNotSetThreshold verifies that SetDefaults
// does NOT set consolidationThreshold when policy is not Balanced.
func TestSetDefaults_NonBalancedPolicyDoesNotSetThreshold(t *testing.T) {
	policies := []ConsolidationPolicy{
		ConsolidationPolicyWhenEmpty,
		ConsolidationPolicyWhenEmptyOrUnderutilized,
	}

	for _, policy := range policies {
		t.Run(string(policy), func(t *testing.T) {
			np := &NodePool{}
			np.Spec.Disruption.ConsolidationPolicy = policy
			np.Spec.Disruption.ConsolidationThreshold = nil

			np.SetDefaults(context.Background())

			if np.Spec.Disruption.ConsolidationThreshold != nil {
				t.Errorf("expected threshold to remain nil for %s policy, got %d", policy, *np.Spec.Disruption.ConsolidationThreshold)
			}
		})
	}
}

// TestSetDefaults_DoesNotOverrideExplicitThreshold verifies that SetDefaults
// does NOT override an explicitly set threshold.
func TestSetDefaults_DoesNotOverrideExplicitThreshold(t *testing.T) {
	np := &NodePool{}
	np.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyBalanced
	np.Spec.Disruption.ConsolidationThreshold = int32Ptr(3)

	np.SetDefaults(context.Background())

	if *np.Spec.Disruption.ConsolidationThreshold != 3 {
		t.Errorf("expected threshold to remain 3, got %d", *np.Spec.Disruption.ConsolidationThreshold)
	}
}

// TestSetDefaults_ExplicitThreshold1Preserved verifies threshold=1 is preserved.
func TestSetDefaults_ExplicitThreshold1Preserved(t *testing.T) {
	np := &NodePool{}
	np.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyBalanced
	np.Spec.Disruption.ConsolidationThreshold = int32Ptr(1)

	np.SetDefaults(context.Background())

	if *np.Spec.Disruption.ConsolidationThreshold != 1 {
		t.Errorf("expected threshold to remain 1, got %d", *np.Spec.Disruption.ConsolidationThreshold)
	}
}
