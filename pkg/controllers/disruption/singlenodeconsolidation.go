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
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/option"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var SingleNodeConsolidationTimeoutDuration = 3 * time.Minute

const SingleNodeConsolidationType = "single"

// SingleNodeConsolidation evaluates one node at a time for consolidation.
type SingleNodeConsolidation struct {
	consolidation
	PreviouslyUnseenNodePools sets.Set[string]
	validator                 Validator
}

func NewSingleNodeConsolidation(c consolidation, opts ...option.Function[MethodOptions]) *SingleNodeConsolidation {
	o := option.Resolve(append([]option.Function[MethodOptions]{WithValidator(NewSingleConsolidationValidator(c))}, opts...)...)
	return &SingleNodeConsolidation{
		consolidation:             c,
		PreviouslyUnseenNodePools: sets.New[string](),
		validator:                 o.validator,
	}
}

// ComputeCommand generates a disruption command given candidates
// nolint:gocyclo
func (s *SingleNodeConsolidation) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	if s.IsConsolidated() {
		return []Command{}, nil
	}
	candidates = s.SortCandidates(ctx, candidates)

	// Set a timeout
	timeout := s.clock.Now().Add(SingleNodeConsolidationTimeoutDuration)
	constrainedByBudgets := false

	unseenNodePools := sets.New(lo.Map(candidates, func(c *Candidate, _ int) string { return c.NodePool.Name })...)

	for i, candidate := range candidates {
		if s.clock.Now().After(timeout) {
			ConsolidationTimeoutsTotal.Inc(map[string]string{ConsolidationTypeLabel: s.ConsolidationType()})
			log.FromContext(ctx).V(1).Info("abandoning single-node consolidation due to timeout", "candidates_evaluated", i)

			s.PreviouslyUnseenNodePools = unseenNodePools

			return []Command{}, nil
		}
		// Track that we've seen this nodepool
		unseenNodePools.Delete(candidate.NodePool.Name)

		// If the disruption budget doesn't allow this candidate to be disrupted,
		// continue to the next candidate. We don't need to decrement any budget
		// counter since single node consolidation commands can only have one candidate.
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			constrainedByBudgets = true
			continue
		}
		// Skip candidates whose best-case score (delete ratio) cannot pass the
		// threshold. A DELETE is the upper bound; if it fails, no REPLACE will pass.
		if !s.evaluator.CanPassThreshold(candidate) {
			continue
		}

		// compute a possible consolidation option
		cmd, err := s.computeConsolidation(ctx, candidate)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed computing consolidation")
			continue
		}
		if cmd.Decision() == NoOpDecision {
			continue
		}
		// Score the move: Balanced pools may reject; other policies pass through.
		if approved, _ := s.evaluator.ApproveCommand(ctx, cmd); !approved {
			continue
		}
		if _, err = s.validator.Validate(ctx, cmd, commandValidationDelay); err != nil {
			if IsValidationError(err) {
				reason := getValidationFailureReason(err)
				cmd.EmitRejectedEvents(s.recorder, reason)
				return []Command{}, nil
			}
			return []Command{}, fmt.Errorf("validating consolidation, %w", err)
		}
		return []Command{cmd}, nil
	}

	if !constrainedByBudgets {
		// if there are no candidates because of a budget, don't mark
		// as consolidated, as it's possible it should be consolidatable
		// the next time we try to disrupt.
		s.markConsolidated()
	}

	s.PreviouslyUnseenNodePools = unseenNodePools

	return []Command{}, nil
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

// SortCandidates applies the consolidation sort, then interweaves by NodePool.
func (s *SingleNodeConsolidation) SortCandidates(ctx context.Context, candidates []*Candidate) []*Candidate {
	candidates = s.sortCandidates(ctx, candidates)
	return s.shuffleCandidates(ctx, lo.GroupBy(candidates, func(c *Candidate) string { return c.NodePool.Name }))
}

func (s *SingleNodeConsolidation) shuffleCandidates(ctx context.Context, nodePoolCandidates map[string][]*Candidate) []*Candidate {
	var result []*Candidate
	// Log any timed out nodepools that we're prioritizing
	if s.PreviouslyUnseenNodePools.Len() != 0 {
		log.FromContext(ctx).V(1).Info("prioritizing nodepools that have not yet been considered due to timeouts in previous runs", "nodepools", strings.Join(s.PreviouslyUnseenNodePools.UnsortedList(), ", "))
	}
	sortedNodePools := s.PreviouslyUnseenNodePools.UnsortedList()
	sortedNodePools = append(sortedNodePools, lo.Filter(lo.Keys(nodePoolCandidates), func(nodePoolName string, _ int) bool {
		return !s.PreviouslyUnseenNodePools.Has(nodePoolName)
	})...)

	// Find the maximum number of candidates in any nodepool
	maxCandidatesPerNodePool := lo.MaxBy(lo.Values(nodePoolCandidates), func(a, b []*Candidate) bool {
		return len(a) > len(b)
	})

	// Interweave candidates from different nodepools
	for i := range maxCandidatesPerNodePool {
		for _, nodePoolName := range sortedNodePools {
			if i < len(nodePoolCandidates[nodePoolName]) {
				result = append(result, nodePoolCandidates[nodePoolName][i])
			}
		}
	}

	return result
}
