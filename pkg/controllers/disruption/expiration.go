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
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// Expiration is a subreconciler that replaces candidates which have outlived their NodePool's expireAfter.
//
// Expiration is a voluntary disruption: the deadline is one Karpenter computes from an operator-set duration and then
// enforces on its own schedule, so unlike a spot reclaim there is nothing external removing the node and there is
// ample warning to stage a replacement. It is therefore paced by the NodePool's disruption budgets under the Expired
// reason, which makes expireAfter a floor on node lifetime rather than a ceiling — a budget allowing no disruptions
// holds an expired candidate until it opens.
//
// Candidates owned by a static NodePool are excluded, mirroring Drift's split with StaticDrift: a static pool sizes
// itself from replicas, so replacement is not staged per-candidate. Those NodeClaims keep the forceful expiration
// path in pkg/controllers/nodeclaim/expiration.
type Expiration struct {
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
	clock       clock.Clock
}

func NewExpiration(kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, recorder events.Recorder, clk clock.Clock) *Expiration {
	return &Expiration{
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
		recorder:    recorder,
		clock:       clk,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (e *Expiration) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	if c.OwnedByStaticNodePool() {
		return false
	}
	return nodeclaimutils.IsExpired(c.NodeClaim, e.clock)
}

// ComputeCommands generates a disruption command given candidates
func (e *Expiration) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	// Sort by expiration deadline, oldest first. An expired candidate never becomes un-expired, so a budget-constrained
	// pool must drain its backlog in deadline order or the longest-lived nodes starve — the opposite of what a maximum
	// node lifetime is for.
	sort.Slice(candidates, func(i int, j int) bool {
		iTime, _ := nodeclaimutils.ExpirationTime(candidates[i].NodeClaim)
		jTime, _ := nodeclaimutils.ExpirationTime(candidates[j].NodeClaim)
		return iTime.Before(jTime)
	})

	emptyCandidates, nonEmptyCandidates := lo.FilterReject(candidates, func(c *Candidate, _ int) bool {
		return len(c.reschedulablePods) == 0
	})

	// Prioritize empty candidates since we want them to get priority over non-empty candidates if the budget is
	// constrained. Disrupting empty candidates first also helps reduce the overall churn because if a non-empty
	// candidate is disrupted first, the pods from that node can reschedule on the empty nodes and will need to move
	// again when those nodes get disrupted.
	for _, candidate := range slices.Concat(emptyCandidates, nonEmptyCandidates) {
		// If the disruption budget doesn't allow this candidate to be disrupted, continue to the next candidate. We
		// don't need to decrement any budget counter since expiration commands can only have one candidate.
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			continue
		}
		// Expiration always replaces first. Terminate-first trades a capacity gap for progress on structurally full
		// pools, which is a deliberate choice drift gates behind a feature gate; expiration has no equivalent gate yet.
		results, _, err := SimulateSchedulingWithReservedFallback(ctx, e.kubeClient, e.cluster, e.provisioner, e.clock, e.recorder, candidate, false, SimulationOptions{})
		if err != nil {
			// if a candidate is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return []Command{}, err
		}
		// Emit an event that we couldn't reschedule the pods on the node.
		if !results.AllNonPendingPodsScheduled() {
			e.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
			continue
		}

		return []Command{{
			Candidates:          []*Candidate{candidate},
			Replacements:        replacementsFromNodeClaims(results.NewNodeClaims...),
			Results:             results,
			PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
		}}, nil
	}
	return []Command{}, nil
}

func (e *Expiration) Reason() v1.DisruptionReason {
	return v1.DisruptionReasonExpired
}

func (e *Expiration) Class() string {
	// Eventual preserves the drain semantics expiration has today: bounded by the NodeClaim's TerminationGracePeriod
	// and not blocked indefinitely by a Node-level do-not-disrupt annotation. Only the pacing changes.
	return EventualDisruptionClass
}

func (e *Expiration) ConsolidationType() string {
	return ""
}
