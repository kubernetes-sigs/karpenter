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
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolbackoff"

	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// StaticDrift is a subreconciler that deletes drifted static candidates, or reboots them when the cloud provider
// resolves their drift in place.
type StaticDrift struct {
	kubeClient    client.Client
	cluster       *state.Cluster
	provisioner   *provisioning.Provisioner
	cloudprovider cloudprovider.CloudProvider
	recorder      events.Recorder
	clock         clock.Clock
	backoff       *nodepoolbackoff.State
}

func NewStaticDrift(kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, cloudprovider cloudprovider.CloudProvider, recorder events.Recorder,
	clk clock.Clock, backoff *nodepoolbackoff.State) *StaticDrift {
	return &StaticDrift{
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		cloudprovider: cloudprovider,
		recorder:      recorder,
		clock:         clk,
		backoff:       backoff,
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
	initDriftBackoffMetrics(ctx, d.backoff, lo.Keys(candidatesByNodePool)...)
	var cmds []Command
	for npName, npCandidates := range candidatesByNodePool {
		np := npCandidates[0].NodePool

		if disruptionBudgetMapping[npName] == 0 {
			continue
		}
		if isDriftBackedOff(ctx, d.backoff, d.recorder, np) {
			continue
		}

		// A reboot keeps the node, so the replicas and limits.nodes checks that gate a replacement don't apply to it.
		rebootCmds, replaceCandidates := d.rebootCommands(ctx, npCandidates, disruptionBudgetMapping[npName])
		cmds = append(cmds, rebootCmds...)
		if len(replaceCandidates) == 0 || len(rebootCmds) >= disruptionBudgetMapping[npName] {
			continue
		}
		npCandidates = replaceCandidates

		limit, ok := np.Spec.Limits[resources.Node]
		nodeLimit := lo.Ternary(ok, limit.Value(), int64(math.MaxInt64))
		// Current nodes (includes in‑flight per your cluster state)
		runningNodes, _, nodesPendingDisruptionCount := d.cluster.NodePoolState.GetNodeCount(npName)

		// We dont want to disrupt nodes until scale down is complete
		if int64(runningNodes+nodesPendingDisruptionCount) > lo.FromPtr(np.Spec.Replicas) {
			continue
		}

		maxDrifts := lo.Min([]int64{
			int64(disruptionBudgetMapping[np.Name] - len(rebootCmds)),
			int64(len(npCandidates)),
		})

		// Full reservations are checked before reserving limits.nodes so the delete-only path never takes a slot. Checked
		// only with terminate-first enabled, since without it there's nothing to do but replace first.
		nct := scheduling.NewNodeClaimTemplate(np)
		if options.FromContext(ctx).FeatureGates.TerminateFirstDrift {
			reservationsFull, offered, err := staticReservations(ctx, d.cloudprovider, np, nct)
			// Other NodePools don't depend on this one, and returning here would discard their commands while keeping the
			// limits.nodes slots they reserved.
			if err != nil {
				log.FromContext(ctx).Error(err, "skipping static nodepool, failed checking capacity reservations")
				continue
			}
			if reservationsFull {
				// Only a candidate holding a slot in a still-offered reservation frees a slot the refill can use, so it goes
				// first and is the only one that can terminate first; the rest can't be replaced either way.
				holdsSlot := func(c *Candidate) bool { return holdsOfferedReservation(c, offered) }
				eligible, rest := lo.FilterReject(npCandidates, func(c *Candidate, _ int) bool { return holdsSlot(c) })
				cmds = append(cmds, d.unreplaceableCommands(ctx, np, append(eligible, rest...)[:maxDrifts], holdsSlot, TerminateFirstNoReservedCapacity,
					"static NodePool's capacity reservations are full and cannot stage a replacement")...)
				continue
			}
		}

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
		// We will not get a negative value here
		if maxAllowedDrifts == 0 {
			cmds = append(cmds, d.unreplaceableCommands(ctx, np, npCandidates[:maxDrifts], func(*Candidate) bool { return true }, TerminateFirstStaticAtLimit,
				"static NodePool is at its node limit and cannot stage a replacement")...)
			continue
		}

		// Select candidates up to maxAllowedDrifts
		for _, c := range npCandidates[:maxAllowedDrifts] {
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

// rebootCommands asks the cloud provider how to resolve each candidate's drift, until reboots use the NodePool's budget.
// It hands the candidates the provider resolves in place to the reboot controller, and returns their commands and the
// candidates to replace. Every candidate to replace is returned, since the replace path picks among them.
func (d *StaticDrift) rebootCommands(ctx context.Context, candidates []*Candidate, budget int) ([]Command, []*Candidate) {
	var cmds []Command
	var replaceCandidates []*Candidate
	for _, c := range candidates {
		if len(cmds) >= budget {
			break
		}
		action, err := driftAction(ctx, d.cloudprovider, c)
		if err != nil {
			log.FromContext(ctx).Error(err, "skipping static drift candidate, failed getting drift status")
			continue
		}
		switch action {
		case cloudprovider.NoOp:
			continue
		case cloudprovider.Reboot:
			// Other NodePools don't depend on this one, and returning an error would discard their commands while keeping the
			// limits.nodes slots they reserved.
			cmd, err := rebootForDrift(ctx, d.kubeClient, d.cluster, d.clock, c)
			if err != nil {
				log.FromContext(ctx).Error(err, "skipping static drift candidate, failed committing reboot")
				continue
			}
			if cmd != nil {
				cmds = append(cmds, *cmd)
			}
			continue
		}
		replaceCandidates = append(replaceCandidates, c)
	}
	return cmds, replaceCandidates
}

// unreplaceableCommands handles a NodePool's candidates when it can't stage a replacement: with TerminateFirstDrift, each
// candidate that canTerminateFirst gets a delete-only command; every other candidate is reported blocked.
func (d *StaticDrift) unreplaceableCommands(ctx context.Context, np *v1.NodePool, candidates []*Candidate, canTerminateFirst func(*Candidate) bool,
	reason TerminateFirstReason, blockedReason string) []Command {
	// Static provisioning refuses NotReady or deleting NodePools, so terminating first there would strand the workload
	// with no replacement.
	terminateFirst := options.FromContext(ctx).FeatureGates.TerminateFirstDrift && np.StatusConditions().Root().IsTrue() && np.DeletionTimestamp.IsZero()
	var cmds []Command
	for _, c := range candidates {
		if terminateFirst && canTerminateFirst(c) {
			cmds = append(cmds, Command{
				Candidates:           []*Candidate{c},
				PoolDisruptionCosts:  computePoolDisruptionCosts([]*Candidate{c}),
				TerminateFirstReason: reason,
			})
			continue
		}
		d.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim, blockedReason)...)
	}
	return cmds
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
