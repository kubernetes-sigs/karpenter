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
	"time"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// DriftIntervalFor returns the drift-detection requeue interval for nodePool.
// If the NodePool specifies a positive DriftInterval in its disruption spec,
// that value is returned; otherwise v1.DefaultDriftInterval is used so that
// existing behavior is preserved.
//
// This function does not itself enforce the 30s minimum that NodePool.Spec.
// Disruption.DriftInterval is subject to (see the XValidation marker on that
// field) - that floor is enforced at CRD admission time. A NodePool object
// that was persisted before the floor existed, or that otherwise ends up
// with a positive-but-sub-floor value, will have that value honored as-is
// here rather than silently clamped, so the requeue cadence always matches
// what's visible on the object.
func DriftIntervalFor(nodePool *v1.NodePool) time.Duration {
	if nodePool == nil {
		return v1.DefaultDriftInterval
	}
	d := nodePool.Spec.Disruption.DriftInterval
	if d != nil && d.Duration > 0 {
		return d.Duration
	}
	return v1.DefaultDriftInterval
}
