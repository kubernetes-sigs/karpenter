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
)

// Karpenter specific taints
const (
	DisruptedTaintKey    = apis.Group + "/disrupted"
	UnregisteredTaintKey = apis.Group + "/unregistered"
	RebootingTaintKey    = apis.Group + "/rebooting"
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
	// RebootingNoScheduleTaint is applied by the reboot controller to fence new scheduling onto a node
	// while its pre-reboot boot may still be active. It is reboot-owned and removed once the node's
	// bootID changes (or on terminal reboot cleanup).
	RebootingNoScheduleTaint = v1.Taint{
		Key:    RebootingTaintKey,
		Effect: v1.TaintEffectNoSchedule,
	}
)
