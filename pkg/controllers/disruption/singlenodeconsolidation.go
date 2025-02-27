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
	"fmt"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
)

const SingleNodeConsolidationTimeoutDuration = 3 * time.Minute
const SingleNodeConsolidationType = "single"

// SingleNodeConsolidation is the consolidation controller that performs single-node consolidation.
type SingleNodeConsolidation struct {
	consolidation
	// nodePoolsTimedOut tracks which nodepools were not fully considered due to timeout
	nodePoolsTimedOut map[string]bool
}

func NewSingleNodeConsolidation(consolidation consolidation) *SingleNodeConsolidation {
	return &SingleNodeConsolidation{
		consolidation:     consolidation,
		nodePoolsTimedOut: make(map[string]bool),
	}
}

func (s *SingleNodeConsolidation) groupCandidatesByNodePool(candidates []*Candidate) (map[string][]*Candidate, []string) {
	nodePoolCandidates := make(map[string][]*Candidate)
	nodePoolNames := []string{}

	for _, candidate := range candidates {
		nodePoolName := candidate.nodePool.Name
		if _, exists := nodePoolCandidates[nodePoolName]; !exists {
			nodePoolNames = append(nodePoolNames, nodePoolName)
		}
		nodePoolCandidates[nodePoolName] = append(nodePoolCandidates[nodePoolName], candidate)
	}
	return nodePoolCandidates, nodePoolNames
}

func (s *SingleNodeConsolidation) sortNodePoolsByTimeout(ctx context.Context, nodePoolNames []string) {
	// Log the timed out nodepools that we're prioritizing
	timedOutNodePools := []string{}
	for np := range s.nodePoolsTimedOut {
		timedOutNodePools = append(timedOutNodePools, np)
	}
	if len(timedOutNodePools) > 0 {
		log.FromContext(ctx).V(1).Info("prioritizing nodepools that have not yet been considered due to timeouts in previous runs: %v", timedOutNodePools)
	}

	// Prioritize nodepools that timed out in previous runs
	sort.Slice(nodePoolNames, func(i, j int) bool {
		// If nodepool i timed out but j didn't, i comes first
		if s.nodePoolsTimedOut[nodePoolNames[i]] && !s.nodePoolsTimedOut[nodePoolNames[j]] {
			return true
		}
		// If nodepool j timed out but i didn't, j comes first
		if !s.nodePoolsTimedOut[nodePoolNames[i]] && s.nodePoolsTimedOut[nodePoolNames[j]] {
			return false
		}
		// If both or neither timed out, keep original order
		return i < j
	})
}

func (s *SingleNodeConsolidation) shuffleCandidates(nodePoolCandidates map[string][]*Candidate, nodePoolNames []string) []*Candidate {
	result := make([]*Candidate, 0)
	maxCandidatesPerNodePool := 0

	// Find the maximum number of candidates in any nodepool
	for _, nodePoolName := range nodePoolNames {
		if len(nodePoolCandidates[nodePoolName]) > maxCandidatesPerNodePool {
			maxCandidatesPerNodePool = len(nodePoolCandidates[nodePoolName])
		}
	}

	// Interweave candidates from different nodepools
	for i := range maxCandidatesPerNodePool {
		for _, nodePoolName := range nodePoolNames {
			if i < len(nodePoolCandidates[nodePoolName]) {
				result = append(result, nodePoolCandidates[nodePoolName][i])
			}
		}
	}

	return result
}

// sortCandidates interweaves candidates from different nodepools and prioritizes nodepools
// that timed out in previous runs
func (s *SingleNodeConsolidation) sortCandidates(candidates []*Candidate) []*Candidate {
	ctx := context.Background()

	// First sort by disruption cost as the base ordering
	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].disruptionCost < candidates[j].disruptionCost
	})

	nodePoolCandidates, nodePoolNames := s.groupCandidatesByNodePool(candidates)

	s.sortNodePoolsByTimeout(ctx, nodePoolNames)

	return s.shuffleCandidates(nodePoolCandidates, nodePoolNames)
}

// ComputeCommand generates a disruption command given candidates
// nolint:gocyclo
func (s *SingleNodeConsolidation) ComputeCommand(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) (Command, scheduling.Results, error) {
	if s.IsConsolidated() {
		return Command{}, scheduling.Results{}, nil
	}
	candidates = s.sortCandidates(candidates)

	v := NewValidation(s.clock, s.cluster, s.kubeClient, s.provisioner, s.cloudProvider, s.recorder, s.queue, s.Reason())

	// Set a timeout
	timeout := s.clock.Now().Add(SingleNodeConsolidationTimeoutDuration)
	constrainedByBudgets := false

	nodePoolsSeen := make(map[string]bool)

	allNodePools := make(map[string]bool)
	for _, candidate := range candidates {
		allNodePools[candidate.nodePool.Name] = true
	}

	for i, candidate := range candidates {
		// Track that we've considered this nodepool
		nodePoolsSeen[candidate.nodePool.Name] = true

		// If the disruption budget doesn't allow this candidate to be disrupted,
		// continue to the next candidate. We don't need to decrement any budget
		// counter since single node consolidation commands can only have one candidate.
		if disruptionBudgetMapping[candidate.nodePool.Name] == 0 {
			constrainedByBudgets = true
			continue
		}
		// Filter out empty candidates. If there was an empty node that wasn't consolidated before this, we should
		// assume that it was due to budgets. If we don't filter out budgets, users who set a budget for `empty`
		// can find their nodes disrupted here.
		if len(candidate.reschedulablePods) == 0 {
			continue
		}
		if s.clock.Now().After(timeout) {
			ConsolidationTimeoutsTotal.Inc(map[string]string{consolidationTypeLabel: s.ConsolidationType()})
			log.FromContext(ctx).V(1).Info(fmt.Sprintf("abandoning single-node consolidation due to timeout after evaluating %d candidates", i))

			// Mark all nodepools that we haven't seen yet as timed out
			for _, c := range candidates[i:] {
				if !nodePoolsSeen[c.nodePool.Name] {
					s.nodePoolsTimedOut[c.nodePool.Name] = true
				}
			}

			return Command{}, scheduling.Results{}, nil
		}
		// compute a possible consolidation option
		cmd, results, err := s.computeConsolidation(ctx, candidate)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed computing consolidation")
			continue
		}
		if cmd.Decision() == NoOpDecision {
			continue
		}
		// might have some edge cases where if there is an error, we should remove the nodepool from the list of "seen" nodepools
		if err := v.IsValid(ctx, cmd, consolidationTTL); err != nil {
			if IsValidationError(err) {
				log.FromContext(ctx).V(1).WithValues(cmd.LogValues()...).Info("abandoning single-node consolidation attempt due to pod churn, command is no longer valid")
				return Command{}, scheduling.Results{}, nil
			}
			return Command{}, scheduling.Results{}, fmt.Errorf("validating consolidation, %w", err)
		}
		return cmd, results, nil
	}

	// Check if we've considered all nodepools
	allNodePoolsConsidered := true
	for nodePool := range allNodePools {
		if !nodePoolsSeen[nodePool] {
			allNodePoolsConsidered = false
			break
		}
	}

	// If we've considered all nodepools, reset the timed out nodepools
	if allNodePoolsConsidered {
		s.nodePoolsTimedOut = make(map[string]bool)
	}

	if !constrainedByBudgets {
		// if there are no candidates because of a budget, don't mark
		// as consolidated, as it's possible it should be consolidatable
		// the next time we try to disrupt.
		s.markConsolidated()
	}
	return Command{}, scheduling.Results{}, nil
}

func (s *SingleNodeConsolidation) Reason() v1.DisruptionReason {
	return v1.DisruptionReasonUnderutilized
}

func (s *SingleNodeConsolidation) Class() string {
	return GracefulDisruptionClass
}

func (s *SingleNodeConsolidation) ConsolidationType() string {
	return SingleNodeConsolidationType
}
