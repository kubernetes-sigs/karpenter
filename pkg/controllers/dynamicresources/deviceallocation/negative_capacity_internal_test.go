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

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
