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

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
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
func (c *EmptyNodeConsolidation) ComputeCommand(ctx context.Context, disruptionBudgetMapping map[string]map[v1beta1.DisruptionReason]int, candidates ...*Candidate) (Command, scheduling.Results, error) {
	if c.IsConsolidated() {
		return Command{}, scheduling.Results{}, nil
	}
	candidates = c.sortCandidates(candidates)

	empty := make([]*Candidate, 0, len(candidates))
	constrainedByBudgets := false
	for _, candidate := range candidates {
		if len(candidate.reschedulablePods) > 0 {
			continue
		}
		if disruptionBudgetMapping[candidate.nodePool.Name][v1beta1.DisruptionReasonEmpty] == 0 {
			// set constrainedByBudgets to true if any node was a candidate but was constrained by a budget
			constrainedByBudgets = true
			continue
		}
		// If there's disruptions allowed for the candidate's nodepool,
		// add it to the list of candidates, and decrement the budget.
		if disruptionBudgetMapping[candidate.nodePool.Name][v1beta1.DisruptionReasonEmpty] > 0 {
			empty = append(empty, candidate)
			disruptionBudgetMapping[candidate.nodePool.Name][v1beta1.DisruptionReasonEmpty]--
		}
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

	v := NewValidation(c.clock, c.cluster, c.kubeClient, c.provisioner, c.cloudProvider, c.recorder, c.queue)
	validatedCandidates, err := v.ValidateCandidates(ctx, cmd.candidates...)
	if err != nil {
		if IsValidationError(err) {
			log.FromContext(ctx).V(1).Info(fmt.Sprintf("abandoning empty node consolidation attempt due to pod churn, command is no longer valid, %s", cmd))
			return Command{}, scheduling.Results{}, nil
		}
		return Command{}, scheduling.Results{}, err
	}

	// TODO (jmdeal@): better encapsulate within validation
	if lo.ContainsBy(validatedCandidates, func(c *Candidate) bool {
		return len(c.reschedulablePods) != 0
	}) {
		log.FromContext(ctx).V(1).Info(fmt.Sprintf("abandoning empty node consolidation attempt due to pod churn, command is no longer valid, %s", cmd))
		return Command{}, scheduling.Results{}, nil
	}

	return cmd, scheduling.Results{}, nil
}

func (c *EmptyNodeConsolidation) Type() string {
	return metrics.ConsolidationReason
}

func (c *EmptyNodeConsolidation) ConsolidationType() string {
	return "empty"
}
