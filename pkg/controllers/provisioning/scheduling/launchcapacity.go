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

package scheduling

import (
	"iter"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// LaunchCapacity is a snapshot of whether a static NodePool's NodeClaim template can launch nodes right now, built from
// the instance types and offerings the cloud provider reports for the NodePool. A static NodeClaim carries exactly the
// template's requirements (the provider resolves the instance type at launch), so this mirrors the launch-time check a
// provider applies to it: some instance type compatible with the template has a compatible, Launchable offering.
//
// It is computed once per NodePool from already-resolved instance types and is independent of the number of nodes.
type LaunchCapacity struct {
	// unbounded is true when a compatible, launchable offering exists that isn't reservation-constrained (on-demand,
	// spot, ...), so any number of launches can be attempted.
	unbounded bool
	// reservedSlots is the remaining free capacity across compatible, launchable reserved offerings.
	reservedSlots int
	// reservations holds the IDs of compatible reserved offerings that are healthy (Available), regardless of their
	// remaining capacity. Terminating a node that holds a slot in one of these frees a slot the template can relaunch
	// into, even when the reservation is currently full.
	reservations sets.Set[string]
}

// NewLaunchCapacity evaluates the NodePool's NodeClaim template against the given instance types (normally the ones
// already resolved for the NodePool this pass). Evaluation stops early once a non-reservation-constrained launchable
// offering is found, since nothing else can change the answer.
func NewLaunchCapacity(nodePool *v1.NodePool, instanceTypes iter.Seq[*cloudprovider.InstanceType]) *LaunchCapacity {
	reqs := NewNodeClaimTemplate(nodePool).Requirements
	// Allocation-free prefilters on the most selective keys; IsCompatible allocates an error for every mismatch, which
	// dominates the cost over a full catalog. They are necessary conditions only, so the result is unchanged.
	instanceTypeReq := reqs.Get(corev1.LabelInstanceTypeStable)
	capacityTypeReq := reqs.Get(v1.CapacityTypeLabelKey)
	compatible := func(r scheduling.Requirements) bool {
		if ct, ok := r[v1.CapacityTypeLabelKey]; ok && !capacityTypeReq.HasIntersection(ct) {
			return false
		}
		return reqs.IsCompatible(r, scheduling.AllowUndefinedWellKnownLabels)
	}
	lc := &LaunchCapacity{reservations: sets.New[string]()}
	slots := map[string]int{}
	for it := range instanceTypes {
		if !instanceTypeReq.Has(it.Name) || !compatible(it.Requirements) {
			continue
		}
		for _, o := range it.Offerings {
			if !o.Available || !compatible(o.Requirements) {
				continue
			}
			req, ok := o.Requirements[v1.CapacityTypeLabelKey]
			if !ok || !req.Has(v1.CapacityTypeReserved) {
				// Available and not reservation-constrained, so it is Launchable with no capacity bound.
				lc.unbounded = true
				return lc
			}
			id := o.ReservationID()
			if id == "" {
				if o.Launchable() {
					lc.reservedSlots += o.ReservationCapacity
				}
				continue
			}
			lc.reservations.Insert(id)
			// A reservation is exposed as one offering per instance type it covers; count its capacity once.
			slots[id] = max(slots[id], o.ReservationCapacity)
		}
	}
	for _, n := range slots {
		lc.reservedSlots += n
	}
	return lc
}

// CanLaunch reports whether at least one more node can be launched from the template right now.
func (lc *LaunchCapacity) CanLaunch() bool {
	return lc.unbounded || lc.reservedSlots > 0
}

// CanRefill reports whether any terminated node could be refilled: either a launch is possible right now, or a healthy
// compatible reservation exists that a terminated node could free a slot in.
func (lc *LaunchCapacity) CanRefill() bool {
	return lc.CanLaunch() || lc.reservations.Len() > 0
}

// Consume accounts for n launches that are already owed to the NodePool (e.g. nodes that are terminating and will be
// refilled) against the finite reserved capacity. It has no effect when capacity is unbounded.
func (lc *LaunchCapacity) Consume(n int) {
	if n <= 0 || lc.unbounded {
		return
	}
	lc.reservedSlots = max(lc.reservedSlots-n, 0)
}

// ClaimRefill reports whether the NodePool can launch a replacement after a node holding a slot in reservationID ("" if
// the node holds no reservation) is terminated, and claims the capacity for it. A node holding a slot in a healthy,
// compatible reservation frees exactly the slot its replacement needs; any other node needs a launchable offering,
// which consumes one unit of finite reserved capacity when no unbounded offering exists.
func (lc *LaunchCapacity) ClaimRefill(reservationID string) bool {
	if reservationID != "" && lc.reservations.Has(reservationID) {
		return true
	}
	if lc.unbounded {
		return true
	}
	if lc.reservedSlots > 0 {
		lc.reservedSlots--
		return true
	}
	return false
}
