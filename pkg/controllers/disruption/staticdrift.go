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
	"maps"
	"math"

	"github.com/samber/lo"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"

	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

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
		// relaunch (see staticRefillCapacity) and Block the rest: a node that is still serving is never traded for a slot
		// that can't be refilled (e.g. its capacity reservation was cancelled or expired, so it now runs as on-demand
		// while the template is reserved-only).
		if options.FromContext(ctx).FeatureGates.TerminateFirstDrift && maxAllowedDrifts == 0 {
			refill, reason := staticRefillCapacity(np, npCandidates[0], int(lo.FromPtr(np.Spec.Replicas))-runningNodes-nodesPendingDisruptionCount)
			if refill == nil {
				for _, c := range npCandidates[:maxDrifts] {
					d.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim, reason)...)
				}
				continue
			}
			// Scan past candidates that can't be refilled (bounded by the budget for Blocked events) so a candidate whose
			// own reservation slot can be reused isn't starved by ones that can't.
			terminating, blocked := int64(0), int64(0)
			for _, c := range npCandidates {
				if terminating == maxDrifts {
					break
				}
				if !refill.ClaimRefill(c.reservationID()) {
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

// staticNoRefillMessage explains why a static candidate wasn't terminated first: no capacity is left to refill it.
const staticNoRefillMessage = "static NodePool is at its node limit and has no launchable capacity to refill this node (e.g. its capacity reservation is gone or full), so it is not terminated first"

// staticRefillCapacity returns the capacity a static NodePool at its node limit has to refill nodes it terminates
// first, or nil and a Blocked reason when it can't refill at all. It is evaluated once per NodePool per pass, from the
// instance types already resolved for the candidates (no cloud provider call), so its cost doesn't grow with the number
// of nodes. owed is the number of nodes the NodePool is already short of its replicas (e.g. still being refilled after
// earlier terminate-first commands); those refills claim reserved capacity first.
func staticRefillCapacity(np *v1.NodePool, c *Candidate, owed int) (*scheduling.LaunchCapacity, string) {
	// Static provisioning refuses NotReady or deleting NodePools, so nothing would refill the freed slot.
	if !np.StatusConditions().Root().IsTrue() || !np.DeletionTimestamp.IsZero() {
		return nil, "static NodePool is at its node limit and is not ready to provision a replacement"
	}
	refill := scheduling.NewLaunchCapacity(np, maps.Values(c.nodePoolInstanceTypes))
	refill.Consume(owed)
	if !refill.CanRefill() {
		return nil, staticNoRefillMessage
	}
	return refill, ""
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
