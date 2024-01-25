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
func (c *EmptyNodeConsolidation) ComputeCommand(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) (Command, error) {
	if c.isConsolidated() {
		return Command{}, nil
	}
	candidates, err := c.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}
	disruptionEligibleNodesGauge.With(map[string]string{
		methodLabel:            c.Type(),
		consolidationTypeLabel: c.ConsolidationType(),
	}).Set(float64(len(candidates)))

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
	if len(empty) == 0 {
		// none empty, so do nothing
		c.markConsolidated()
		return Command{}, nil
	}

	cmd := Command{
		candidates: empty,
	}

	// Empty Node Consolidation doesn't use Validation as we get to take advantage of cluster.IsNodeNominated.  This
	// lets us avoid a scheduling simulation (which is performed periodically while pending pods exist and drives
	// cluster.IsNodeNominated already).
	select {
	case <-ctx.Done():
		return Command{}, errors.New("interrupted")
	case <-c.clock.After(consolidationTTL):
	}
	validationCandidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, c.ShouldDisrupt, c.queue)
	if err != nil {
		logging.FromContext(ctx).Errorf("computing validation candidates %s", err)
		return Command{}, err
	}
	// Get the current representation of the proposed candidates from before the validation timeout
	// We do this so that we can re-validate that the candidates that were computed before we made the decision are the same
	candidatesToDelete := mapCandidates(cmd.candidates, validationCandidates)

	postValidationMapping, err := BuildDisruptionBudgets(ctx, c.cluster, c.clock, c.kubeClient, c.recorder)
	if err != nil {
		return Command{}, fmt.Errorf("building disruption budgets, %w", err)
	}

	// The deletion of empty NodeClaims is easy to validate, we just ensure that:
	// 1. All the candidatesToDelete are still empty
	// 2. The node isn't a target of a recent scheduling simulation
	// 3. the number of candidates for a given nodepool can no longer be disrupted as it would violate the budget
	for _, n := range candidatesToDelete {
		if len(n.pods) != 0 || c.cluster.IsNodeNominated(n.ProviderID()) || postValidationMapping[n.nodePool.Name] == 0 {
			logging.FromContext(ctx).Debugf("abandoning empty node consolidation attempt due to pod churn, command is no longer valid, %s", cmd)
			return Command{}, nil
		}
		postValidationMapping[n.nodePool.Name]--
	}
	return cmd, nil
}

func (c *EmptyNodeConsolidation) Type() string {
	return metrics.ConsolidationReason
}

func (c *EmptyNodeConsolidation) ConsolidationType() string {
	return "empty"
}
