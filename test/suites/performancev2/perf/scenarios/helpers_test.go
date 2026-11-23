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

package scenarios

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestFrac(t *testing.T) {
	for _, tc := range []struct{ n, num, den, want int }{{1000, 35, 1000, 35}, {100, 35, 1000, 3}, {10, 1, 11, 1}, {0, 1, 2, 1}} {
		if got := frac(tc.n, tc.num, tc.den); got != tc.want {
			t.Errorf("frac(%d, %d, %d) = %d, want %d", tc.n, tc.num, tc.den, got, tc.want)
		}
	}
}

func TestObjects(t *testing.T) {
	if got := objects([]*corev1.Pod{{}, {}}); len(got) != 2 {
		t.Errorf("objects returned %d objects, want 2", len(got))
	}
}
