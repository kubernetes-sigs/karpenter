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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestDriftIntervalFor(t *testing.T) {
	for _, tt := range []struct {
		name     string
		nodePool *v1.NodePool
		want     time.Duration
	}{
		{
			name:     "nil NodePool falls back to the default",
			nodePool: nil,
			want:     v1.DefaultDriftInterval,
		},
		{
			name:     "nil DriftInterval falls back to the default",
			nodePool: &v1.NodePool{},
			want:     v1.DefaultDriftInterval,
		},
		{
			name: "zero Duration falls back to the default",
			nodePool: &v1.NodePool{Spec: v1.NodePoolSpec{Disruption: v1.Disruption{
				DriftInterval: &metav1.Duration{Duration: 0},
			}}},
			want: v1.DefaultDriftInterval,
		},
		{
			name: "negative Duration falls back to the default",
			nodePool: &v1.NodePool{Spec: v1.NodePoolSpec{Disruption: v1.Disruption{
				DriftInterval: &metav1.Duration{Duration: -time.Minute},
			}}},
			want: v1.DefaultDriftInterval,
		},
		{
			// DriftIntervalFor intentionally does not itself re-enforce the 30s floor
			// that's applied by the NodePool CRD's XValidation rule on this field
			// (spec.disruption.driftInterval) at admission time. A value below the
			// floor should never actually reach this function in a cluster where the
			// admission check is in effect, but if it somehow does (e.g. an object
			// persisted before the floor existed), this function honors the value on
			// the object as-is rather than silently clamping it, so the requeue
			// cadence always matches what's visible on the NodePool.
			name: "a positive value below the 30s admission floor is passed through unchanged",
			nodePool: &v1.NodePool{Spec: v1.NodePoolSpec{Disruption: v1.Disruption{
				DriftInterval: &metav1.Duration{Duration: 10 * time.Second},
			}}},
			want: 10 * time.Second,
		},
		{
			name: "a valid override interval is honored",
			nodePool: &v1.NodePool{Spec: v1.NodePoolSpec{Disruption: v1.Disruption{
				DriftInterval: &metav1.Duration{Duration: 90 * time.Second},
			}}},
			want: 90 * time.Second,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := DriftIntervalFor(tt.nodePool); got != tt.want {
				t.Errorf("DriftIntervalFor() = %v, want %v", got, tt.want)
			}
		})
	}
}
