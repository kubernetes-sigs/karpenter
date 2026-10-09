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
	"context"
	"math"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	pscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const staticNoRefillMessage = "static NodePool is at its node limit and has no launchable capacity to refill this node; it is not terminated first"

// staticRefill tracks whether a static NodePool at its node limit can refill the nodes it terminates first.
type staticRefill struct {
	unbounded    bool           // a compatible, available non-reserved offering (on-demand, spot) exists
	reservations map[string]int // compatible, healthy reservation ID -> free capacity
	free         int            // free reserved capacity not yet claimed by a refill
}

// newStaticRefill evaluates the NodePool's template against its already-resolved instance types. pendingRefills is the
// number of nodes the NodePool is already short of its replicas; those refills claim free reserved capacity first.
func newStaticRefill(np *v1.NodePool, instanceTypes map[string]*cloudprovider.InstanceType, pendingRefills int) *staticRefill {
	reqs := scheduling.NewNodeClaimTemplate(np).Requirements
	instanceTypeReq, capacityTypeReq := reqs.Get(corev1.LabelInstanceTypeStable), reqs.Get(v1.CapacityTypeLabelKey)
	compatible := func(req pscheduling.Requirements) bool {
		capacityType, ok := req[v1.CapacityTypeLabelKey]
		return (!ok || capacityTypeReq.HasIntersection(capacityType)) && reqs.IsCompatible(req, pscheduling.AllowUndefinedWellKnownLabels)
	}
	r := &staticRefill{reservations: map[string]int{}}
	for _, it := range instanceTypes {
		if !instanceTypeReq.Has(it.Name) || !compatible(it.Requirements) {
			continue
		}
		for _, o := range it.Offerings {
			if !o.Available || !compatible(o.Requirements) {
				continue
			}
			if capacityType, ok := o.Requirements[v1.CapacityTypeLabelKey]; !ok || !capacityType.Has(v1.CapacityTypeReserved) {
				r.unbounded = true
				return r
			}
			r.reservations[o.ReservationID()] = o.ReservationCapacity
		}
	}
	r.free = lo.Sum(lo.Values(r.reservations)) - pendingRefills
	return r
}

// claim reports whether the NodePool can refill c once it is terminated, and claims the capacity for it. A node holding
// a slot in a healthy reservation frees the slot its own refill needs, even if the reservation is full.
func (r *staticRefill) claim(c *Candidate) bool {
	if _, ok := r.reservations[c.reservationID()]; r.unbounded || ok {
		return true
	}
	if r.free > 0 {
		r.free--
		return true
	}
	return false
}

// StaticDrift is a subreconciler that deletes drifted static candidates.
type StaticDrift struct {
	cluster       *state.Cluster
	provisioner   *provisioning.Provisioner
	cloudprovider cloudprovider.CloudProvider
	recorder      events.Recorder
}

func NewStaticDrift(cluster *state.Cluster, provisioner *provisioning.Provisioner, cloudprovider cloudprovider.CloudProvider, recorder events.Recorder) *StaticDrift {
	return &StaticDrift{
		cluster:       cluster,
		provisioner:   provisioner,
		cloudprovider: cloudprovider,
		recorder:      recorder,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (d *StaticDrift) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	return c.OwnedByStaticNodePool() && c.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()
}

func (d *StaticDrift) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	// Group candidates by nodepool name
	candidatesByNodePool := lo.GroupBy(candidates, func(candidate *Candidate) string {
		return candidate.NodePool.Name
	})

	var cmds []Command
	for npName, npCandidates := range candidatesByNodePool {
		np := npCandidates[0].NodePool

		if disruptionBudgetMapping[npName] == 0 {
			continue
		}

		limit, ok := np.Spec.Limits[resources.Node]
		nodeLimit := lo.Ternary(ok, limit.Value(), int64(math.MaxInt64))
		// Current nodes (includes in‑flight per your cluster state)
		runningNodes, _, nodesPendingDisruptionCount := d.cluster.NodePoolState.GetNodeCount(npName)

		// We dont want to disrupt nodes until scale down is complete
		if int64(runningNodes+nodesPendingDisruptionCount) > lo.FromPtr(np.Spec.Replicas) {
			continue
		}

		maxDrifts := lo.Min([]int64{
			int64(disruptionBudgetMapping[np.Name]),
			int64(len(npCandidates)),
		})

		// Acquire limits from cluster state without bursting over. maxAllowedDrifts is how many candidates we can drift
		// while staging a replacement for each without exceeding the NodePool's node limit; 0 means the pool is at its
		// limit and can't stage any replacement.
		maxAllowedDrifts := d.cluster.NodePoolState.ReserveNodeCount(npName, nodeLimit, maxDrifts)

		// Terminate-first (RFC #3203): when the NodePool is at its node limit it can't stage a replacement first — a
		// pre-spun replacement would be an (N+1)th node the operator capped out. Issue budget-paced delete-only commands;
		// once the freed slot is released the static.provisioning controller refills the pool back to Spec.Replicas. The
		// drain still honors PDBs and is bounded by TGP. When the pool has room under its limit, fall through to the
		// normal replace-first path below. No replacement is reserved for terminate-first, so the reservation above is a
		// no-op in that case (it reserved nothing).
		//
		// Terminating first only helps if the freed slot can be refilled, so only terminate candidates the NodePool can
		// refill and block the rest.
		if options.FromContext(ctx).FeatureGates.TerminateFirstDrift && maxAllowedDrifts == 0 {
			if !np.StatusConditions().Root().IsTrue() || !np.DeletionTimestamp.IsZero() {
				for _, c := range npCandidates[:maxDrifts] {
					d.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim, "static NodePool is at its node limit and cannot stage a replacement")...)
				}
				continue
			}
			refill := newStaticRefill(np, npCandidates[0].nodePoolInstanceTypes, int(lo.FromPtr(np.Spec.Replicas))-runningNodes-nodesPendingDisruptionCount)
			// Scan past candidates that can't be refilled (bounded by the budget for Blocked events) so a candidate whose
			// own reservation slot can be reused isn't starved by ones that can't.
			terminating, blocked := int64(0), int64(0)
			for _, c := range npCandidates {
				if terminating == maxDrifts {
					break
				}
				if !refill.claim(c) {
					if blocked < maxDrifts {
						d.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim, staticNoRefillMessage)...)
						blocked++
					}
					continue
				}
				terminating++
				cmds = append(cmds, Command{
					Candidates:          []*Candidate{c},
					PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{c}),
					TerminateFirst:      true,
				})
			}
			continue
		}

		// We will not get a negative value here
		if maxAllowedDrifts == 0 {
			for _, c := range npCandidates[:maxDrifts] {
				d.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim, "static NodePool is at its node limit and cannot stage a replacement")...)
			}
			continue
		}

		// Select candidates up to maxAllowedDrifts
		for _, c := range npCandidates[:maxAllowedDrifts] {
			nct := scheduling.NewNodeClaimTemplate(np)
			result := scheduling.Results{
				NewNodeClaims: []*scheduling.NodeClaim{{NodeClaimTemplate: *nct}},
			}
			cmds = append(cmds, Command{
				Candidates:          []*Candidate{c},
				Replacements:        replacementsFromNodeClaims(result.NewNodeClaims...),
				Results:             result,
				PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{c}),
			})
		}
	}
	return cmds, nil
}

func (d *StaticDrift) Reason() v1.DisruptionReason {
	return v1.DisruptionReasonDrifted
}

func (d *StaticDrift) Class() string {
	return EventualDisruptionClass
}

func (d *StaticDrift) ConsolidationType() string {
	return ""
}
