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

package deviceallocation

import (
	"testing"
	"unique"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// TestHasNegativeCapacity is an internal unit test (no envtest) for the sign scan that flags an invalid
// consumed-capacity contribution. A negative in any dimension invalidates the whole contribution so the
// device fails closed, rather than keeping the other dimensions (which would read as zero usage). See #3209.
func TestHasNegativeCapacity(t *testing.T) {
	cases := []struct {
		name string
		cap  map[resourcev1.QualifiedName]resource.Quantity
		want bool
	}{
		{"nil", nil, false},
		{"empty", map[resourcev1.QualifiedName]resource.Quantity{}, false},
		{"positive", map[resourcev1.QualifiedName]resource.Quantity{"memory": resource.MustParse("256Mi")}, false},
		{"zero is not negative", map[resourcev1.QualifiedName]resource.Quantity{"memory": resource.MustParse("0")}, false},
		{"negative", map[resourcev1.QualifiedName]resource.Quantity{"memory": resource.MustParse("-1")}, true},
		{"negative among positive dimensions", map[resourcev1.QualifiedName]resource.Quantity{
			"memory":      resource.MustParse("-128Mi"),
			"connections": resource.MustParse("3"),
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasNegativeCapacity(tc.cap); got != tc.want {
				t.Fatalf("hasNegativeCapacity(%v) = %v, want %v", tc.cap, got, tc.want)
			}
		})
	}
}

// TestContributionsForResults exercises the ingestion folding rules directly with constructed results, so
// the negative-handling and share-aggregation behavior is covered without relying on the API server
// accepting a negative ConsumedCapacity (kubernetes/kubernetes#140563 may reject it in a future envtest).
func TestContributionsForResults(t *testing.T) {
	id := func(device string) cloudprovider.DeviceID {
		return cloudprovider.DeviceID{Driver: unique.Make("driver"), Pool: unique.Make("pool"), Device: unique.Make(device)}
	}
	result := func(device string, cap map[resourcev1.QualifiedName]resource.Quantity) resourcev1.DeviceRequestAllocationResult {
		return resourcev1.DeviceRequestAllocationResult{Driver: "driver", Pool: "pool", Device: device, Request: "req", ConsumedCapacity: cap}
	}
	neg := map[resourcev1.QualifiedName]resource.Quantity{"memory": resource.MustParse("-1")}
	pos2 := map[resourcev1.QualifiedName]resource.Quantity{"memory": resource.MustParse("2")}
	pos3 := map[resourcev1.QualifiedName]resource.Quantity{"memory": resource.MustParse("3")}

	t.Run("a negative result flags the device invalid", func(t *testing.T) {
		got := contributionsForResults([]resourcev1.DeviceRequestAllocationResult{result("d0", neg)})
		if !got[id("d0")].InvalidConsumedCapacity {
			t.Fatal("want InvalidConsumedCapacity=true")
		}
	})
	t.Run("a valid sibling result does not clear the invalid flag", func(t *testing.T) {
		got := contributionsForResults([]resourcev1.DeviceRequestAllocationResult{result("d0", pos2), result("d0", neg)})
		if !got[id("d0")].InvalidConsumedCapacity {
			t.Fatal("want InvalidConsumedCapacity=true even with a valid sibling result on the same device")
		}
	})
	t.Run("distinct shares of the same device are summed", func(t *testing.T) {
		got := contributionsForResults([]resourcev1.DeviceRequestAllocationResult{result("d0", pos2), result("d0", pos3)})
		c := got[id("d0")]
		if c.InvalidConsumedCapacity {
			t.Fatal("want InvalidConsumedCapacity=false")
		}
		if q := c.ConsumedCapacity["memory"]; q.Cmp(resource.MustParse("5")) != 0 {
			t.Fatalf("want summed memory=5, got %s", q.String())
		}
	})
	t.Run("only the malformed device is flagged", func(t *testing.T) {
		got := contributionsForResults([]resourcev1.DeviceRequestAllocationResult{result("d0", neg), result("d1", pos2)})
		if !got[id("d0")].InvalidConsumedCapacity {
			t.Fatal("d0 should be invalid")
		}
		if got[id("d1")].InvalidConsumedCapacity {
			t.Fatal("d1 should not be invalid")
		}
		if q := got[id("d1")].ConsumedCapacity["memory"]; q.Cmp(resource.MustParse("2")) != 0 {
			t.Fatalf("d1 memory want 2, got %s", q.String())
		}
	})
}
