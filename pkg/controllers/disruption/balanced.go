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
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
)

// BalancedScoreResult holds the result of scoring a consolidation move.
type BalancedScoreResult struct {
	Score                  float64
	SavingsFraction        float64
	DisruptionFraction     float64
	Approved               bool
	Threshold              float64
	ConsolidationThreshold int32
}

// NodePoolTotals holds the precomputed totals for a NodePool needed by the scoring function.
type NodePoolTotals struct {
	TotalCost           float64
	TotalDisruptionCost float64
}

// computeNodePoolTotals computes NodePool totals from the full set of candidates
// before any ShouldDisrupt filtering, so that balanced scoring normalizes against
// the entire NodePool. Candidates with no resolvable price (nil instanceType or
// no compatible offerings) are excluded from both cost and disruption totals to
// avoid skewing the ratio.
func computeNodePoolTotals(ctx context.Context, allCandidates []*Candidate) map[string]NodePoolTotals {
	totalsMap := map[string]NodePoolTotals{}
	for _, c := range allCandidates {
		price := candidatePrice(c)
		if price == 0 {
			continue
		}
		name := c.NodePool.Name
		totals := totalsMap[name]
		totals.TotalCost += price
		totals.TotalDisruptionCost += 1.0 // per-node base
		for _, p := range c.reschedulablePods {
			evictionCost := disruptionutils.EvictionCost(ctx, p)
			totals.TotalDisruptionCost += math.Max(0, evictionCost)
		}
		totalsMap[name] = totals
	}
	return totalsMap
}

// candidatePrice returns the cheapest compatible offering price for a single
// candidate. Returns 0 if the candidate has no instance type or no compatible
// offerings (e.g., deprecated instance types -- drift should handle those).
func candidatePrice(c *Candidate) float64 {
	if c == nil || c.instanceType == nil {
		return 0
	}
	reqs := scheduling.NewLabelRequirements(c.Labels())
	offerings := c.instanceType.Offerings.Compatible(reqs)
	if len(offerings) == 0 {
		return 0
	}
	return offerings.Cheapest().Price
}

// ScoreMove scores a consolidation move using the balanced scoring formula.
//
//	savings_fraction = savings / nodepool_total_cost
//	disruption_fraction = disruption_cost / nodepool_total_disruption_cost
//	score = savings_fraction / disruption_fraction
//
// A move is approved when score >= 1/consolidationThreshold.
func ScoreMove(savings float64, disruptionCost float64, totals NodePoolTotals, consolidationThreshold int32) BalancedScoreResult {
	// Zero nodepool cost: no consolidation possible
	if totals.TotalCost <= 0 {
		return BalancedScoreResult{Score: 0, Approved: false}
	}

	savingsFraction := savings / totals.TotalCost

	// Zero savings: never approved
	if savings <= 0 {
		return BalancedScoreResult{
			Score:           0,
			SavingsFraction: savingsFraction,
			Approved:        false,
		}
	}

	// Zero total disruption cost: any move with positive savings is approved
	if totals.TotalDisruptionCost <= 0 {
		return BalancedScoreResult{
			Score:           math.Inf(1),
			SavingsFraction: savingsFraction,
			Approved:        savings > 0,
		}
	}

	disruptionFraction := disruptionCost / totals.TotalDisruptionCost
	score := savingsFraction / disruptionFraction
	threshold := 1.0 / float64(consolidationThreshold)

	return BalancedScoreResult{
		Score:                  score,
		SavingsFraction:        savingsFraction,
		DisruptionFraction:     disruptionFraction,
		Approved:               score >= threshold,
		Threshold:              threshold,
		ConsolidationThreshold: consolidationThreshold,
	}
}

// ComputeMoveDisruptionCost computes the disruption cost for a consolidation move.
// It adds a per-node base of 1.0 plus sum(max(0, EvictionCost(pod))) for all
// reschedulable pods on the candidate nodes.
//
// This does not include LifetimeRemaining adjustment. Candidate ordering already
// uses lifetime-adjusted DisruptionCost, so nodes near expiration sort first.
// Scoring evaluates the static cost structure. The two compose: lifetime affects
// which node is tried, scoring affects whether the move is worth it.
//
// To justify adding lifetime adjustment here, we would need evidence that Balanced
// rejects moves on near-expiration nodes that should be approved. Analysis shows
// the crossover is at lifetime_remaining=0.0045 for the marginal replace case
// (29.9 days into a 30-day expireAfter), where expiration handles the node within
// hours. At k=2, zero moves flip when lifetime >= 25%. See designs/balanced-consolidation.md
// "Resolved Questions" section.
func ComputeMoveDisruptionCost(ctx context.Context, candidates []*Candidate) float64 {
	cost := float64(len(candidates)) // per-node base of 1.0
	for _, c := range candidates {
		for _, p := range c.reschedulablePods {
			evictionCost := disruptionutils.EvictionCost(ctx, p)
			cost += math.Max(0, evictionCost)
		}
	}
	return cost
}

// GetConsolidationThreshold returns the consolidation threshold for a NodePool,
// defaulting to DefaultConsolidationThreshold if not set.
func GetConsolidationThreshold(nodePool *v1.NodePool) int32 {
	if nodePool.Spec.Disruption.ConsolidationThreshold != nil {
		return *nodePool.Spec.Disruption.ConsolidationThreshold
	}
	return v1.DefaultConsolidationThreshold
}

// effectiveBalanced returns true when the NodePool uses the Balanced
// consolidation policy AND the BalancedConsolidation feature gate is enabled.
// When the gate is disabled, Balanced is treated as if it were
// WhenEmptyOrUnderutilized so the controller falls back to the prior
// consolidation behavior rather than refusing to act on the pool. The status
// condition ConsolidationPolicyUnsupported, set in ShouldDisrupt, communicates
// the fallback to operators.
func effectiveBalanced(ctx context.Context, np *v1.NodePool) bool {
	if np.Spec.Disruption.ConsolidationPolicy != v1.ConsolidationPolicyBalanced {
		return false
	}
	return options.FromContext(ctx).FeatureGates.BalancedConsolidation
}

// AnyBalancedCandidate returns true if any candidate in the list uses the
// Balanced consolidation policy with the BalancedConsolidation feature gate
// enabled. This handles cross-NodePool batches where only some candidates may
// use Balanced, and it honors the feature gate so the rest of the controller
// path applies WhenEmptyOrUnderutilized semantics when the gate is off.
func AnyBalancedCandidate(ctx context.Context, candidates []*Candidate) bool {
	return lo.SomeBy(candidates, func(c *Candidate) bool {
		return effectiveBalanced(ctx, c.NodePool)
	})
}

// EvaluateBalancedMove evaluates whether a consolidation command should be
// approved under the Balanced policy. For cross-NodePool moves, each source
// NodePool with Balanced policy is scored independently using its own totals
// and tolerance. The move is approved only if ALL Balanced pools approve.
func EvaluateBalancedMove(ctx context.Context, cmd Command, nodePoolTotals map[string]NodePoolTotals) BalancedScoreResult {
	if len(cmd.Candidates) == 0 {
		return BalancedScoreResult{Score: 0, Approved: false}
	}

	// Group candidates by NodePool
	byPool := lo.GroupBy(cmd.Candidates, func(c *Candidate) string { return c.NodePool.Name })

	// Total savings for the whole command
	savings := cmd.EstimatedSavings()

	// Score per Balanced NodePool. Each pool must independently approve.
	var worstResult BalancedScoreResult
	worstResult.Approved = true
	worstResult.Score = math.Inf(1)

	for poolName, poolCandidates := range byPool {
		nodePool := poolCandidates[0].NodePool
		// Filter to Balanced pools. Gate gating happens at the entry point
		// in AnyBalancedCandidate, which is the only path that reaches here.
		if nodePool.Spec.Disruption.ConsolidationPolicy != v1.ConsolidationPolicyBalanced {
			continue
		}

		consolidationThreshold := GetConsolidationThreshold(nodePool)
		disruptionCost := ComputeMoveDisruptionCost(ctx, poolCandidates)
		totals := nodePoolTotals[poolName]

		// For cross-NodePool moves, attribute savings proportionally to each
		// pool's share of the total source cost
		poolCost := candidatesCost(poolCandidates)
		totalCost := candidatesCost(cmd.Candidates)
		poolSavings := savings
		if totalCost > 0 && len(byPool) > 1 {
			poolSavings = savings * (poolCost / totalCost)
		}

		result := ScoreMove(poolSavings, disruptionCost, totals, consolidationThreshold)

		log.FromContext(ctx).V(1).Info("balanced consolidation score",
			"nodepool", poolName,
			"score", result.Score,
			"savings_fraction", result.SavingsFraction,
			"disruption_fraction", result.DisruptionFraction,
			"savings", poolSavings,
			"disruption_cost", disruptionCost,
			"nodepool_total_cost", totals.TotalCost,
			"nodepool_total_disruption_cost", totals.TotalDisruptionCost,
			"threshold", 1.0/float64(consolidationThreshold),
			"approved", result.Approved,
			"decision", cmd.Decision(),
			"candidates", lo.Map(poolCandidates, func(c *Candidate, _ int) string { return c.Name() }),
		)

		if !result.Approved {
			return result
		}
		if result.Score < worstResult.Score {
			worstResult = result
		}
	}

	return worstResult
}

// EmitBalancedMultiNodeEvents emits events and metrics for a final multi-node
// command, one per Balanced NodePool in the batch. This ensures cross-NodePool
// batches get events on each participating pool, not just the first.
func EmitBalancedMultiNodeEvents(ctx context.Context, cmd Command, nodePoolTotals map[string]NodePoolTotals, recorder events.Recorder) {
	byPool := lo.GroupBy(cmd.Candidates, func(c *Candidate) string { return c.NodePool.Name })
	savings := cmd.EstimatedSavings()

	for poolName, poolCandidates := range byPool {
		nodePool := poolCandidates[0].NodePool
		// Filter to Balanced pools. Gate gating happens at the entry point.
		if nodePool.Spec.Disruption.ConsolidationPolicy != v1.ConsolidationPolicyBalanced {
			continue
		}

		consolidationThreshold := GetConsolidationThreshold(nodePool)
		disruptionCost := ComputeMoveDisruptionCost(ctx, poolCandidates)
		totals := nodePoolTotals[poolName]

		poolCost := candidatesCost(poolCandidates)
		totalCost := candidatesCost(cmd.Candidates)
		poolSavings := savings
		if totalCost > 0 && len(byPool) > 1 {
			poolSavings = savings * (poolCost / totalCost)
		}

		result := ScoreMove(poolSavings, disruptionCost, totals, consolidationThreshold)

		decisionLabel := "approved"
		if !result.Approved {
			decisionLabel = "rejected"
		}
		ConsolidationScoreHistogram.Observe(result.Score, map[string]string{"decision": decisionLabel, "nodepool": poolName})
		ConsolidationMovesTotal.Inc(map[string]string{"decision": decisionLabel, "nodepool": poolName})

		if result.Approved {
			recorder.Publish(disruptionevents.BalancedConsolidationApprovedMultiNode(
				nodePool,
				result.Score, result.Threshold, result.ConsolidationThreshold,
				result.SavingsFraction*100, result.DisruptionFraction*100,
			))
		} else {
			recorder.Publish(disruptionevents.BalancedConsolidationRejectedMultiNode(
				nodePool,
				result.Score, result.Threshold, result.ConsolidationThreshold,
				result.SavingsFraction*100, result.DisruptionFraction*100,
			))
		}
	}
}

// candidatesCost returns the total price of a set of candidates.
func candidatesCost(candidates []*Candidate) float64 {
	cost := 0.0
	for _, c := range candidates {
		cost += candidatePrice(c)
	}
	return cost
}
