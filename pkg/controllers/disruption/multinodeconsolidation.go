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
	"math"
	"slices"
	"time"

	"github.com/awslabs/operatorpkg/option"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	scheduler "sigs.k8s.io/karpenter/pkg/scheduling"
)

const MultiNodeConsolidationTimeoutDuration = 1 * time.Minute
const MultiNodeConsolidationType = "multi"

// MultiNodePairwiseSearchTimeoutDuration bounds the latency added by
// the pairwise non-prefix fallback (see karpenter#1962). The fallback
// only runs when the primary binary search returns no plan, so this
// budget is consumed only on cases the existing search misses. With
// computeConsolidation latency typically 10 to 30ms, 10s allows
// roughly 300 to 1000 probes, enough for the largest practical batch
// (max=100) on a healthy cluster. Regret on overloaded clusters where
// each probe is slower stays bounded by this budget.
const MultiNodePairwiseSearchTimeoutDuration = 10 * time.Second

type MultiNodeConsolidation struct {
	consolidation
	validator Validator
}

func NewMultiNodeConsolidation(c consolidation, opts ...option.Function[MethodOptions]) *MultiNodeConsolidation {
	o := option.Resolve(append([]option.Function[MethodOptions]{WithValidator(NewMultiConsolidationValidator(c))}, opts...)...)
	return &MultiNodeConsolidation{
		consolidation: c,
		validator:     o.validator,
	}
}

// nolint:gocyclo
func (m *MultiNodeConsolidation) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	if m.IsConsolidated() {
		return []Command{}, nil
	}
	candidates = m.sortCandidates(candidates)

	// In order, filter out all candidates that would violate the budget.
	// Since multi-node consolidation relies on the ordering of
	// these candidates, and does computation in batches of these nodes by
	// simulateScheduling(nodes[0, n]), doing a binary search on n to find
	// the optimal consolidation command, this pre-filters out nodes that
	// would have violated the budget anyway, preserving the ordering
	// and only considering a number of nodes that can be disrupted.
	disruptableCandidates := make([]*Candidate, 0, len(candidates))
	constrainedByBudgets := false
	for _, candidate := range candidates {
		// If there's disruptions allowed for the candidate's nodepool,
		// add it to the list of candidates, and decrement the budget.
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			constrainedByBudgets = true
			continue
		}
		// Filter out empty candidates. If there was an empty node that wasn't consolidated before this, we should
		// assume that it was due to budgets. If we don't filter out budgets, users who set a budget for `empty`
		// can find their nodes disrupted here.
		if len(candidate.reschedulablePods) == 0 {
			continue
		}
		// set constrainedByBudgets to true if any node was a candidate but was constrained by a budget
		disruptableCandidates = append(disruptableCandidates, candidate)
		disruptionBudgetMapping[candidate.NodePool.Name]--
	}

	// Only consider a maximum batch of 100 NodeClaims to save on computation.
	// This could be further configurable in the future.
	maxParallel := lo.Clamp(len(disruptableCandidates), 0, 100)

	cmd, err := m.firstNConsolidationOption(ctx, disruptableCandidates, maxParallel)
	if err != nil {
		return []Command{}, err
	}

	if cmd.Decision() == NoOpDecision {
		// if there are no candidates because of a budget, don't mark
		// as consolidated, as it's possible it should be consolidatable
		// the next time we try to disrupt.
		if !constrainedByBudgets {
			m.markConsolidated()
		}
		return []Command{}, nil
	}

	if cmd, err = m.validator.Validate(ctx, cmd, commandValidationDelay); err != nil {
		if IsValidationError(err) {
			reason := getValidationFailureReason(err)
			cmd.EmitRejectedEvents(m.recorder, reason)
			return []Command{}, nil
		}
		return []Command{}, fmt.Errorf("validating consolidation, %w", err)
	}
	return []Command{cmd}, nil
}

// firstNConsolidationOption looks at the first N NodeClaims to determine if they can all be consolidated at once.
// It runs the binary search over sorted prefixes first (existing
// behavior, fast log(N) probes) and falls back to the pairwise
// non-prefix search only when the binary search returns no plan.
// This preserves equivalence with prior versions on every input the
// binary search already handled, and adds the pairwise recovery only
// for inputs where binary search exits empty (the karpenter#1962
// shape, where a candidate that blocks joint deletion sits early in
// the sort order and poisons every prefix probe).
//
// Latency contract:
//   - Binary search: bounded by MultiNodeConsolidationTimeoutDuration (1m).
//   - Pairwise fallback: bounded by MultiNodePairwiseSearchTimeoutDuration (10s).
//
// On a successful binary search no extra latency is paid. On a
// 1962-shaped failure the additional cost is at most the pairwise
// timeout, regardless of N. Big-cluster regret is bounded by that
// 10s budget.
func (m *MultiNodeConsolidation) firstNConsolidationOption(ctx context.Context, candidates []*Candidate, max int) (Command, error) {
	// we always operate on at least two NodeClaims at once, for single NodeClaims standard consolidation will find all solutions
	if len(candidates) < 2 {
		return Command{}, nil
	}

	primary, err := m.binarySearchPrefix(ctx, candidates, max)
	if err != nil {
		return Command{}, err
	}
	if primary.Decision() != NoOpDecision {
		// Existing behavior: binary search found a feasible prefix.
		return primary, nil
	}

	// Binary search exhausted without finding a feasible prefix
	// (the karpenter#1962 case). Fall back to the pairwise non-prefix
	// walk, bounded by its own timeout so the latency added on big
	// clusters stays capped.
	return m.pairwiseSearchFallback(ctx, candidates, max)
}

// binarySearchPrefix is the prior firstNConsolidationOption logic:
// binary search over sorted prefixes [0:mid+1], doubling on success
// and halving on failure. Preserved verbatim so every input the
// existing search handled keeps producing the same result.
//
// nolint:gocyclo
func (m *MultiNodeConsolidation) binarySearchPrefix(ctx context.Context, candidates []*Candidate, max int) (Command, error) {
	min := 1
	if len(candidates) <= max {
		max = len(candidates) - 1
	}

	lastSavedCommand := Command{}
	// Set a timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, MultiNodeConsolidationTimeoutDuration)
	defer cancel()
	for min <= max {
		mid := (min + max) / 2
		candidatesToConsolidate := candidates[0 : mid+1]

		// Pass the timeout context to ensure sub-operations can be canceled
		cmd, err := m.computeConsolidation(timeoutCtx, candidatesToConsolidate...)
		// context deadline exceeded will return to the top of the loop and either return nothing or the last saved command
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				ConsolidationTimeoutsTotal.Inc(map[string]string{ConsolidationTypeLabel: m.ConsolidationType()})
				if lastSavedCommand.Candidates == nil {
					log.FromContext(ctx).V(1).Info("failed to find a multi-node consolidation after timeout", "last_batch_size", (min+max)/2)
					return Command{}, nil
				}
				log.FromContext(ctx).V(1).WithValues(lastSavedCommand.LogValues()...).Info("stopping multi-node consolidation after timeout, returning last valid command")
				return lastSavedCommand, nil

			}
			return Command{}, err
		}
		// ensure that the action is sensical for replacements, see explanation on filterOutSameType for why this is
		// required
		validDecision := cmd.Decision() == DeleteDecision
		if cmd.Decision() == ReplaceDecision {
			cmd.Replacements[0], err = filterOutSameInstanceType(cmd.Replacements[0], candidatesToConsolidate)
			// we check the error before the replacement instanceTypeOptions since we return nil for the replacement if we get an error
			if err == nil && len(cmd.Replacements[0].InstanceTypeOptions) > 0 {
				validDecision = true
			}
		}
		if validDecision {
			// We can consolidate NodeClaims [0,mid]
			lastSavedCommand = cmd
			min = mid + 1
		} else {
			max = mid - 1
		}
	}
	return lastSavedCommand, nil
}

// pairwiseSearchFallback walks candidates in order, simulating the
// joint deletion of (already accepted plus this candidate) at each
// step. Accepts the candidate when the simulator returns feasible,
// skips it otherwise and continues. Non-prefix subsets are reachable
// because skipping does not narrow the search. This is the recovery
// for karpenter#1962.
//
// Bounded by MultiNodePairwiseSearchTimeoutDuration so the latency
// added when this fallback runs is capped independently of N. On
// timeout, returns the last valid command (possibly empty).
//
// nolint:gocyclo
func (m *MultiNodeConsolidation) pairwiseSearchFallback(ctx context.Context, candidates []*Candidate, max int) (Command, error) {
	if len(candidates) > max {
		candidates = candidates[:max+1]
	}

	lastSavedCommand := Command{}
	timeoutCtx, cancel := context.WithTimeout(ctx, MultiNodePairwiseSearchTimeoutDuration)
	defer cancel()

	accepted := make([]*Candidate, 0, len(candidates))

	for _, c := range candidates {
		// Bail out early if the pairwise timeout has fired before
		// we evaluate the next candidate.
		if err := timeoutCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				ConsolidationTimeoutsTotal.Inc(map[string]string{ConsolidationTypeLabel: m.ConsolidationType()})
				if lastSavedCommand.Candidates == nil {
					log.FromContext(ctx).V(1).Info("pairwise fallback found no consolidation before timeout", "accepted_size", len(accepted))
					return Command{}, nil
				}
				log.FromContext(ctx).V(1).WithValues(lastSavedCommand.LogValues()...).Info("pairwise fallback stopping after timeout, returning last valid command")
				return lastSavedCommand, nil
			}
			return Command{}, err
		}

		candidatesToConsolidate := append(slices.Clone(accepted), c)

		// When accepted is empty this probes a single candidate, which
		// single-node consolidation already evaluated. The probe is still
		// needed: the walk uses the result to decide whether to accept or
		// skip c. Without it, a bad first candidate would poison the
		// entire accepted set with no way to eject it.
		cmd, err := m.computeConsolidation(timeoutCtx, candidatesToConsolidate...)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				ConsolidationTimeoutsTotal.Inc(map[string]string{ConsolidationTypeLabel: m.ConsolidationType()})
				if lastSavedCommand.Candidates == nil {
					log.FromContext(ctx).V(1).Info("pairwise fallback found no consolidation before timeout", "accepted_size", len(accepted))
					return Command{}, nil
				}
				log.FromContext(ctx).V(1).WithValues(lastSavedCommand.LogValues()...).Info("pairwise fallback stopping after timeout, returning last valid command")
				return lastSavedCommand, nil
			}
			return Command{}, err
		}

		// ensure that the action is sensical for replacements, see explanation on filterOutSameInstanceType for why this is
		// required
		validDecision := cmd.Decision() == DeleteDecision
		if cmd.Decision() == ReplaceDecision {
			cmd.Replacements[0], err = filterOutSameInstanceType(cmd.Replacements[0], candidatesToConsolidate)
			if err == nil && len(cmd.Replacements[0].InstanceTypeOptions) > 0 {
				validDecision = true
			}
		}

		if validDecision {
			// Joint deletion of (accepted ∪ c) is feasible. Promote
			// the candidate set and remember the command. Continue
			// walking; later candidates may compose with this set.
			accepted = candidatesToConsolidate
			lastSavedCommand = cmd
			continue
		}
		// Joint deletion failed with c included. Skip c and try the
		// next candidate. Skipping (rather than narrowing) is what
		// lets the search reach non-prefix subsets.
	}
	return lastSavedCommand, nil
}

// filterOutSameInstanceType filters out instance types that are more expensive than the cheapest instance type that is being
// consolidated if the list of replacement instance types include one of the instance types that is being removed
//
// This handles the following potential consolidation result:
// NodeClaims=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.small, t3a.xlarge, t3a.2xlarge
//
// In this case, we shouldn't perform this consolidation at all.  This is equivalent to just
// deleting the 2x t3a.xlarge NodeClaims.  This code will identify that t3a.small is in both lists and filter
// out any instance type that is the same or more expensive than the t3a.small
//
// For another scenario:
// NodeClaims=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.nano, t3a.small, t3a.xlarge, t3a.2xlarge
//
// This code sees that t3a.small is the cheapest type in both lists and filters it and anything more expensive out
// leaving the valid consolidation:
// NodeClaims=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.nano
func filterOutSameInstanceType(replacement *Replacement, consolidate []*Candidate) (*Replacement, error) {
	existingInstanceTypes := sets.New[string]()
	pricesByInstanceType := map[string]float64{}

	// get the price of the cheapest node that we currently are considering deleting indexed by instance type
	for _, c := range consolidate {
		existingInstanceTypes.Insert(c.instanceType.Name)
		compatibleOfferings := c.instanceType.Offerings.Compatible(scheduler.NewLabelRequirements(c.Labels()))
		if len(compatibleOfferings) == 0 {
			continue
		}
		existingPrice, ok := pricesByInstanceType[c.instanceType.Name]
		if !ok {
			existingPrice = math.MaxFloat64
		}
		if p := compatibleOfferings.Cheapest().Price; p < existingPrice {
			pricesByInstanceType[c.instanceType.Name] = p
		}
	}

	maxPrice := math.MaxFloat64
	for _, it := range replacement.InstanceTypeOptions {
		// we are considering replacing multiple NodeClaims with a single NodeClaim of one of the same types, so the replacement
		// node must be cheaper than the price of the existing node, or we should just keep that one and do a
		// deletion only to reduce cluster disruption (fewer pods will re-schedule).
		if existingInstanceTypes.Has(it.Name) {
			if pricesByInstanceType[it.Name] < maxPrice {
				maxPrice = pricesByInstanceType[it.Name]
			}
		}
	}
	var err error
	replacement.NodeClaim, err = replacement.RemoveInstanceTypeOptionsByPriceAndMinValues(replacement.Requirements, maxPrice)
	if err != nil {
		return nil, err
	}
	return replacement, nil
}

func (m *MultiNodeConsolidation) Reason() v1.DisruptionReason {
	return v1.DisruptionReasonUnderutilized
}

func (m *MultiNodeConsolidation) Class() string {
	return GracefulDisruptionClass
}

func (m *MultiNodeConsolidation) ConsolidationType() string {
	return MultiNodeConsolidationType
}
