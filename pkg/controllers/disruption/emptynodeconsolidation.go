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

	"knative.dev/pkg/logging"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// EmptyNodeConsolidation is the consolidation controller that performs multi-nodeclaim consolidation of entirely empty nodes
type EmptyNodeConsolidation struct {
	consolidation
}

func NewEmptyNodeConsolidation(consolidation consolidation) *EmptyNodeConsolidation {
	return &EmptyNodeConsolidation{consolidation: consolidation}
}

// ComputeCommand generates a disruption command given candidates
//
//nolint:gocyclo
func (c *EmptyNodeConsolidation) ComputeCommand(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) (Command, scheduling.Results, error) {
	if c.IsConsolidated() {
		return Command{}, scheduling.Results{}, nil
	}
	candidates = c.sortCandidates(candidates)
	EligibleNodesGauge.With(map[string]string{
		methodLabel:            c.Type(),
		consolidationTypeLabel: c.ConsolidationType(),
	}).Set(float64(len(candidates)))

	empty := make([]*Candidate, 0, len(candidates))
	constrainedByBudgets := false
	for _, candidate := range candidates {
		if len(candidate.reschedulablePods) > 0 {
			continue
		}
		if disruptionBudgetMapping[candidate.nodePool.Name] == 0 {
			// set constrainedByBudgets to true if any node was a candidate but was constrained by a budget
			constrainedByBudgets = true
			continue
		}
		// If there's disruptions allowed for the candidate's nodepool,
		// add it to the list of candidates, and decrement the budget.
		empty = append(empty, candidate)
		disruptionBudgetMapping[candidate.nodePool.Name]--
	}
	// none empty, so do nothing
	if len(empty) == 0 {
		// if there are no candidates, but a nodepool had a fully blocking budget,
		// don't mark the cluster as consolidated, as it's possible this nodepool
		// should be consolidated the next time we try to disrupt.
		if !constrainedByBudgets {
			c.markConsolidated()
		}
		return Command{}, scheduling.Results{}, nil
	}

	cmd := Command{
		candidates: empty,
	}

	// Empty Node Consolidation doesn't use Validation as we get to take advantage of cluster.IsNodeNominated.  This
	// lets us avoid a scheduling simulation (which is performed periodically while pending pods exist and drives
	// cluster.IsNodeNominated already).
	select {
	case <-ctx.Done():
		return Command{}, scheduling.Results{}, errors.New("interrupted")
	case <-c.clock.After(consolidationTTL):
	}
	validationCandidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.clock, c.cloudProvider, c.ShouldDisrupt, c.queue)
	if err != nil {
		logging.FromContext(ctx).Errorf("computing validation candidates %s", err)
		return Command{}, scheduling.Results{}, err
	}
	// Get the current representation of the proposed candidates from before the validation timeout
	// We do this so that we can re-validate that the candidates that were computed before we made the decision are the same
	candidatesToDelete := mapCandidates(cmd.candidates, validationCandidates)

	postValidationMapping, err := BuildDisruptionBudgets(ctx, c.cluster, c.clock, c.kubeClient)
	if err != nil {
		return Command{}, scheduling.Results{}, fmt.Errorf("building disruption budgets, %w", err)
	}

	// The deletion of empty NodeClaims is easy to validate, we just ensure that:
	// 1. All the candidatesToDelete are still empty
	// 2. The node isn't a target of a recent scheduling simulation
	// 3. the number of candidates for a given nodepool can no longer be disrupted as it would violate the budget
	for _, n := range candidatesToDelete {
		if len(n.reschedulablePods) != 0 || c.cluster.IsNodeNominated(n.ProviderID()) || postValidationMapping[n.nodePool.Name] == 0 {
			logging.FromContext(ctx).Debugf("abandoning empty node consolidation attempt due to pod churn, command is no longer valid, %s", cmd)
			return Command{}, scheduling.Results{}, nil
		}
		postValidationMapping[n.nodePool.Name]--
	}
	return cmd, scheduling.Results{}, nil
}

func (c *EmptyNodeConsolidation) Type() string {
	return metrics.ConsolidationReason
}

func (c *EmptyNodeConsolidation) ConsolidationType() string {
	return "empty"
}
