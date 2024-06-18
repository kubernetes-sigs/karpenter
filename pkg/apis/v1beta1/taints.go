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

package v1beta1

import (
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
)

// Karpenter specific taints
const (
	DisruptionTaintKey             = apis.Group + "/disruption"
	DisruptingNoScheduleTaintValue = "disrupting"
	DisruptionCandidateTaintKey    = Group + "/candidate"
)

var (
	// DisruptionNoScheduleTaint is used by the deprovisioning controller to ensure no pods
	// are scheduled to a node that Karpenter is actively disrupting.
	DisruptionNoScheduleTaint = v1.Taint{
		Key:    DisruptionTaintKey,
		Effect: v1.TaintEffectNoSchedule,
		Value:  DisruptingNoScheduleTaintValue,
	}

	DisruptionCandidatePreferNoScheduleTaint = v1.Taint{
		Key:    DisruptionCandidateTaintKey,
		Effect: v1.TaintEffectPreferNoSchedule,
	}

	// DisruptionCandidateNoScheduleTaint is applied in-memory to drifted and expired nodes.
	// This, along with the PreferNoSchedule taint applied to all disruption candidates, ensures
	// that we don't reschedule nodes on disruption candidates to other disruption candidates.
	// Note: this taint is **only** applied in-memory. It does not get applied to nodes on the
	// API server.
	DisruptionCandidateNoScheduleTaint = v1.Taint{
		Key:    DisruptionCandidateTaintKey,
		Effect: v1.TaintEffectNoSchedule,
	}
)

func IsDisruptingTaint(taint v1.Taint) bool {
	return taint.MatchTaint(&DisruptionNoScheduleTaint) && taint.Value == DisruptingNoScheduleTaintValue
}
