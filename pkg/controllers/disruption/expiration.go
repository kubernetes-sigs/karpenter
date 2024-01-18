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
	"fmt"
	"sort"

	"k8s.io/utils/clock"

	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// Expiration is a subreconciler that deletes empty candidates.
// Expiration will respect TTLSecondsAfterEmpty
type Expiration struct {
	clock       clock.Clock
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
}

func NewExpiration(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, recorder events.Recorder) *Expiration {
	return &Expiration{
		clock:       clk,
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
		recorder:    recorder,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (e *Expiration) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	return c.nodePool.Spec.Disruption.ExpireAfter.Duration != nil &&
		c.NodeClaim.StatusConditions().GetCondition(v1beta1.Expired).IsTrue()
}

// SortCandidates orders expired candidates by when they've expired
func (e *Expiration) filterAndSortCandidates(ctx context.Context, candidates []*Candidate) ([]*Candidate, error) {
	candidates, err := filterCandidates(ctx, e.kubeClient, e.recorder, candidates)
	if err != nil {
		return nil, fmt.Errorf("filtering candidates, %w", err)
	}
	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].NodeClaim.StatusConditions().GetCondition(v1beta1.Expired).LastTransitionTime.Inner.Time.Before(
			candidates[j].NodeClaim.StatusConditions().GetCondition(v1beta1.Expired).LastTransitionTime.Inner.Time)
	})
	return candidates, nil
}

// ComputeCommand generates a disrpution command given candidates
func (e *Expiration) ComputeCommand(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) (Command, error) {
	candidates, err := e.filterAndSortCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("filtering candidates, %w", err)
	}
	disruptionEligibleNodesGauge.With(map[string]string{
		methodLabel:            e.Type(),
		consolidationTypeLabel: e.ConsolidationType(),
	}).Set(float64(len(candidates)))

	// Do a quick check through the candidates to see if they're empty.
	// For each candidate that is empty with a nodePool allowing its disruption
	// add it to the existing command.
	empty := make([]*Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if len(candidate.pods) > 0 {
			continue
		}
		// If there's disruptions allowed for the candidate's nodepool,
		// add it to the list of candidates, and decrement the budget.
		if disruptionBudgetMapping[candidate.nodePool.Name] > 0 {
			empty = append(empty, candidate)
			disruptionBudgetMapping[candidate.nodePool.Name]--
		}
	}
	// Disrupt all empty expired candidates, as they require no scheduling simulations.
	if len(empty) > 0 {
		return Command{
			candidates: empty,
		}, nil
	}

	for _, candidate := range candidates {
		// If the disruption budget doesn't allow this candidate to be disrupted,
		// continue to the next candidate. We don't need to decrement any budget
		// counter since expiration commands can only have one candidate.
		if disruptionBudgetMapping[candidate.nodePool.Name] == 0 {
			continue
		}
		// Check if we need to create any NodeClaims.
		results, err := simulateScheduling(ctx, e.kubeClient, e.cluster, e.provisioner, candidate)
		if err != nil {
			// if a candidate node is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return Command{}, err
		}
		// Emit an event that we couldn't reschedule the pods on the node.
		if !results.AllNonPendingPodsScheduled() {
			e.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, "Scheduling simulation failed to schedule all pods")...)
			continue
		}

		logging.FromContext(ctx).With("ttl", candidates[0].nodePool.Spec.Disruption.ExpireAfter.String()).Infof("triggering termination for expired node after TTL")
		return Command{
			candidates:   []*Candidate{candidate},
			replacements: results.NewNodeClaims,
		}, nil
	}
	return Command{}, nil
}

func (e *Expiration) Type() string {
	return metrics.ExpirationReason
}

func (e *Expiration) ConsolidationType() string {
	return ""
}
