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
	"errors"
	"slices"
	"sort"

	"github.com/samber/lo"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolbackoff"
)

// Drift is a subreconciler that deletes drifted candidates.
type Drift struct {
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
	clock       clock.Clock
	backoff     *nodepoolbackoff.State
}

func NewDrift(kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, recorder events.Recorder, clk clock.Clock, backoff *nodepoolbackoff.State) *Drift {
	return &Drift{
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
		recorder:    recorder,
		clock:       clk,
		backoff:     backoff,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (d *Drift) ShouldDisrupt(ctx context.Context, c *Candidate) bool {
	return !c.OwnedByStaticNodePool() && c.NodeClaim.StatusConditions().Get(string(d.Reason())).IsTrue()
}

// ComputeCommand generates a disruption command given candidates
func (d *Drift) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	initDriftBackoffMetrics(ctx, d.backoff, lo.Uniq(lo.Map(candidates, func(c *Candidate, _ int) string { return c.NodePool.Name }))...)

	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].NodeClaim.StatusConditions().Get(string(d.Reason())).LastTransitionTime.Time.Before(
			candidates[j].NodeClaim.StatusConditions().Get(string(d.Reason())).LastTransitionTime.Time)
	})

	emptyCandidates, nonEmptyCandidates := lo.FilterReject(candidates, func(c *Candidate, _ int) bool {
		return len(c.reschedulablePods) == 0
	})

	// Prioritize empty candidates since we want them to get priority over non-empty candidates if the budget is constrained.
	// Disrupting empty candidates first also helps reduce the overall churn because if a non-empty candidate is disrupted first,
	// the pods from that node can reschedule on the empty nodes and will need to move again when those nodes get disrupted.
	for _, candidate := range slices.Concat(emptyCandidates, nonEmptyCandidates) {
		// If the disruption budget doesn't allow this candidate to be disrupted,
		// continue to the next candidate. We don't need to decrement any budget
		// counter since drift commands can only have one candidate.
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			continue
		}
		if isDriftBackedOff(ctx, d.backoff, d.recorder, candidate.NodePool) {
			continue
		}
		// Simulate rescheduling the candidate's pods. When they can't be replaced-first and the candidate holds a full
		// reservation, this reports terminate-first (RFC #3203): delete the candidate and let reactive provisioning
		// refill the freed slot. See SimulateSchedulingWithReservedFallback.
		results, terminateFirst, err := SimulateSchedulingWithReservedFallback(ctx, d.kubeClient, d.cluster, d.provisioner, d.clock, d.recorder, candidate, options.FromContext(ctx).FeatureGates.TerminateFirstDrift, SimulationOptions{})
		if err != nil {
			// if a candidate is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return []Command{}, err
		}
		if terminateFirst {
			// Delete-only (no Replacements): carry the Results so existing nodes that can absorb the freed pods get
			// nominated. Reactive provisioning handles the rest.
			return []Command{{
				Candidates:          []*Candidate{candidate},
				Results:             results,
				PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
				TerminateFirst:      true,
			}}, nil
		}
		// Emit an event that we couldn't reschedule the pods on the node.
		if !results.AllNonPendingPodsScheduled() {
			d.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
			continue
		}

		cmd := Command{
			Candidates:          []*Candidate{candidate},
			Replacements:        replacementsFromNodeClaims(results.NewNodeClaims...),
			Results:             results,
			PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
		}
		return []Command{cmd}, nil

	}
	return []Command{}, nil
}

func (d *Drift) Reason() v1.DisruptionReason {
	return v1.DisruptionReasonDrifted
}

func (d *Drift) Class() string {
	return EventualDisruptionClass
}

func (d *Drift) ConsolidationType() string {
	return ""
}

func driftBackoffEnabled(ctx context.Context, backoff *nodepoolbackoff.State) bool {
	return options.FromContext(ctx).FeatureGates.NodePoolDriftBackoff && backoff != nil
}

// initDriftBackoffMetrics registers a zero-valued back-off counter for each NodePool so the metric is visible (at 0)
// for healthy pools rather than being absent until the first back-off. Add(0) is idempotent: it only ensures the
// series exists and never clobbers an incremented value.
func initDriftBackoffMetrics(ctx context.Context, backoff *nodepoolbackoff.State, nodePoolNames ...string) {
	if !driftBackoffEnabled(ctx, backoff) {
		return
	}
	for _, nodePoolName := range nodePoolNames {
		DriftBackoffsTotal.Add(0, map[string]string{metrics.NodePoolLabel: nodePoolName})
	}
}

// isDriftBackedOff reports whether the NodePool is currently backed off after repeated unrecoverable drift replacement
// failures, publishing an event when it is.
func isDriftBackedOff(ctx context.Context, backoff *nodepoolbackoff.State, recorder events.Recorder, nodePool *v1.NodePool) bool {
	if !driftBackoffEnabled(ctx, backoff) {
		return false
	}
	level, until, backedOff := backoff.GetBackoff(nodePool)
	if backedOff {
		recorder.Publish(disruptionevents.NodePoolDriftBackoff(nodePool, until, level))
	}
	return backedOff
}
