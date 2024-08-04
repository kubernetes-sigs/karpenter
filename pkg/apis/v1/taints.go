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
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
)

// Karpenter specific taints
const (
	DisruptedTaintKey    = apis.Group + "/disrupted"
	UnregisteredTaintKey = apis.Group + "/unregistered"
)

var (
	// DisruptedNoScheduleTaint is applied by the disruption and termination controllers to nodes disrupted by Karpenter.
	// This ensures no additional pods schedule to those nodes while they are terminating.
	DisruptedNoScheduleTaint = v1.Taint{
		Key:    DisruptedTaintKey,
		Effect: v1.TaintEffectNoSchedule,
	}
	UnregisteredNoExecuteTaint = v1.Taint{
		Key:    UnregisteredTaintKey,
		Effect: v1.TaintEffectNoExecute,
	}
)

// IsDisruptingTaint checks if the taint is either the v1 or v1beta1 disruption taint.
func IsDisruptingTaint(taint v1.Taint) bool {
	return taint.MatchTaint(&DisruptedNoScheduleTaint) || v1beta1.IsDisruptingTaint(taint)
}
