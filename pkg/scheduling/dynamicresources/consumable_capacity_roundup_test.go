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

package dynamicresources

import (
	"math"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func qBin(v int64) *resource.Quantity { return resource.NewQuantity(v, resource.BinarySI) }

func qStr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestRoundUpRange(t *testing.T) {
	cases := []struct {
		name    string
		request *resource.Quantity
		min     *resource.Quantity
		step    *resource.Quantity
		// exactly one of wantValue / wantReturnsRequest applies
		wantValue          int64
		wantReturnsRequest bool
	}{
		{name: "normal rounds up", request: qBin(5), min: qBin(0), step: qBin(2), wantValue: 6},
		{name: "below min returns min", request: qBin(1), min: qBin(4), step: qBin(2), wantValue: 4},
		// The next valid point is already on the grid and representable, keep it.
		{name: "on grid at MaxInt64", request: qBin(math.MaxInt64), min: qBin(1), step: qBin(2), wantValue: math.MaxInt64},
		// Exercises n++ and the equality boundary of the overflow guard (must not over-reject).
		{name: "largest safe round-up (n++ to MaxInt64)", request: qBin(math.MaxInt64 - 1), min: qBin(1), step: qBin(2), wantValue: math.MaxInt64},
		// Final Min + N*Step overflows int64: reject rather than wrap negative.
		{name: "final add overflow rejected", request: qBin(math.MaxInt64), min: qBin(0), step: qBin(2), wantReturnsRequest: true},
		// Request above MaxInt64 (Value() would truncate to a negative int64).
		{name: "request above MaxInt64 rejected", request: qStr("9223372036854775808"), min: qBin(0), step: qBin(2), wantReturnsRequest: true},
		// Step of 2^64 truncates to 0 with Value(); 2^64+1 truncates to 1.
		{name: "huge step 2^64 rejected", request: qBin(8), min: qBin(0), step: qStr("18446744073709551616"), wantReturnsRequest: true},
		{name: "huge step 2^64+1 not treated as 1", request: qBin(8), min: qBin(0), step: qStr("18446744073709551617"), wantReturnsRequest: true},
		{name: "zero step rejected", request: qBin(8), min: qBin(0), step: qBin(0), wantReturnsRequest: true},
		{name: "negative step rejected", request: qBin(8), min: qBin(0), step: qBin(-2), wantReturnsRequest: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got resource.Quantity
			mustNotPanic(t, "roundUpRange", func() {
				got = roundUpRange(tc.request, &resourcev1.CapacityRequestPolicyRange{Min: tc.min, Step: tc.step})
			})
			// The consumed capacity must never be negative (that under-counts and over-admits).
			if got.Sign() < 0 {
				t.Fatalf("roundUpRange returned a negative value: %s", got.String())
			}
			if tc.wantReturnsRequest {
				if got.Cmp(*tc.request) != 0 {
					t.Fatalf("expected the request %s returned unchanged, got %s", tc.request.String(), got.String())
				}
				return
			}
			if got.Value() != tc.wantValue {
				t.Fatalf("got %d, want %d", got.Value(), tc.wantValue)
			}
		})
	}
}

func TestViolateValidRangeFailsClosed(t *testing.T) {
	// A malformed Step (zero, negative, or above MaxInt64) must be reported as a
	// violation so violatesPolicy rejects it, rather than being silently ignored.
	for _, step := range []*resource.Quantity{qBin(0), qBin(-2), qStr("18446744073709551616")} {
		mustNotPanic(t, "violateValidRange", func() {
			if !violateValidRange(*qBin(8), resourcev1.CapacityRequestPolicyRange{Min: qBin(0), Step: step}) {
				t.Fatalf("expected malformed step %s to be a violation", step.String())
			}
		})
	}
	// A value on the grid is not a violation.
	if violateValidRange(*qBin(6), resourcev1.CapacityRequestPolicyRange{Min: qBin(0), Step: qBin(2)}) {
		t.Fatalf("6 should be on the grid min=0 step=2")
	}
}

func mustNotPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", name, r)
		}
	}()
	fn()
}

// TestCheckCapacityRejectsNegativePersistedSource asserts checkCapacity fails
// closed when a persisted source (for example an anomalous ConsumedCapacity
// ingested into PreallocatedConsumedCapacity without validation) is negative, which
// would otherwise offset the used total and over-admit the device.
func TestCheckCapacityRejectsNegativePersistedSource(t *testing.T) {
	capName := resourcev1.QualifiedName("memory")
	did := DeviceID{}
	dev := cloudprovider.Device{
		AllowMultipleAllocations: true,
		Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			capName: {Value: *qBin(4)},
		},
	}
	rd := &RequestData{CapacityRequests: map[resourcev1.QualifiedName]resource.Quantity{capName: *qBin(5)}}
	negative := func() map[DeviceID]map[resourcev1.QualifiedName]resource.Quantity {
		return map[DeviceID]map[resourcev1.QualifiedName]resource.Quantity{did: {capName: *qBin(-1)}}
	}
	// A negative quantity in any individual non-template source must fail closed;
	// otherwise it credits capacity and over-admits (used = -1 + 5 = 4 on capacity 4).
	sources := map[string]func(*allocator){
		"PreallocatedConsumedCapacity": func(a *allocator) { a.allocationTracker.PreallocatedConsumedCapacity = negative() },
		"InflightConsumedCapacity":     func(a *allocator) { a.allocationTracker.InflightConsumedCapacity = negative() },
		"allocatingCapacity":           func(a *allocator) { a.allocatingCapacity = negative() },
	}
	for name, inject := range sources {
		t.Run(name, func(t *testing.T) {
			a := &allocator{Allocator: &Allocator{allocationTracker: &AllocationTracker{}}}
			inject(a)
			if _, ok := a.checkCapacity(dev, did, rd); ok {
				t.Fatalf("checkCapacity accepted a request with a negative %s credit", name)
			}
		})
	}
}

// TestComputeConsumedCapacityRejectsMalformedRangeWithMatchingDefault asserts a
// structurally malformed ValidRange is rejected even when the consumed value equals
// RequestPolicy.Default, so the Default short-circuit cannot mask it.
func TestComputeConsumedCapacityRejectsMalformedRangeWithMatchingDefault(t *testing.T) {
	capName := resourcev1.QualifiedName("memory")
	withRange := func(vr *resourcev1.CapacityRequestPolicyRange) map[resourcev1.QualifiedName]resourcev1.DeviceCapacity {
		return map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			capName: {Value: *qBin(4), RequestPolicy: &resourcev1.CapacityRequestPolicy{Default: qBin(4), ValidRange: vr}},
		}
	}
	// An empty request fills the Default (4); the malformed range must still be rejected.
	if _, err := computeConsumedCapacity(nil, withRange(&resourcev1.CapacityRequestPolicyRange{Step: qBin(1)})); err == nil {
		t.Fatalf("a nil-Min range should be rejected even when consumed matches Default")
	}
	if _, err := computeConsumedCapacity(nil, withRange(&resourcev1.CapacityRequestPolicyRange{Min: qBin(0), Step: qBin(0)})); err == nil {
		t.Fatalf("a zero-Step range should be rejected even when consumed matches Default")
	}
}

// TestComputeConsumedCapacityRejectsNegative drives the real computeConsumedCapacity
// path and asserts a negative consumed capacity (from a negative request with no
// policy, or a negative RequestPolicy.Default for an empty request) is rejected
// rather than stored as a negative credit that would over-admit the device.
func TestComputeConsumedCapacityRejectsNegative(t *testing.T) {
	capName := resourcev1.QualifiedName("memory")
	noPolicy := map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
		capName: {Value: *qBin(4)},
	}
	if _, err := computeConsumedCapacity(
		map[resourcev1.QualifiedName]resource.Quantity{capName: *qBin(-1)}, noPolicy); err == nil {
		t.Fatalf("a negative request should be rejected")
	}
	negDefault := map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
		capName: {Value: *qBin(4), RequestPolicy: &resourcev1.CapacityRequestPolicy{Default: qBin(-1)}},
	}
	if _, err := computeConsumedCapacity(nil, negDefault); err == nil {
		t.Fatalf("a negative RequestPolicy.Default should be rejected")
	}
	consumed, err := computeConsumedCapacity(
		map[resourcev1.QualifiedName]resource.Quantity{capName: *qBin(2)}, noPolicy)
	if err != nil {
		t.Fatalf("a normal request should be accepted: %v", err)
	}
	if got := consumed[capName]; got.Value() != 2 {
		t.Fatalf("consumed = %d, want 2", got.Value())
	}
}

func TestViolateValidRangeNilMinNoPanic(t *testing.T) {
	mustNotPanic(t, "violateValidRange with nil Min", func() {
		if !violateValidRange(*qBin(4), resourcev1.CapacityRequestPolicyRange{Step: qBin(1)}) {
			t.Fatalf("a Step with a nil Min should be treated as a violation")
		}
	})
}
