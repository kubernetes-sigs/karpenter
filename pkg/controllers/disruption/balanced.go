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

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/state/cost"
)

// NodePoolTotals holds precomputed cost and disruption sums for a NodePool.
type NodePoolTotals struct {
	TotalCost           float64
	TotalDisruptionCost float64
}

// computeNodePoolTotals builds NodePoolTotals for each pool represented in
// allCandidates. TotalCost is read from ClusterCost (precomputed cluster state)
// when available, falling back to summing candidate prices. TotalDisruptionCost
// is computed from ALL nodes in the pool (not just candidates), per the RFC:
// "Non-candidate nodes still contribute to the denominators."
//
// For candidate nodes, we use the accurate RescheduleDisruptionCost already
// computed from pod objects. For non-candidate nodes, we use the exact
// disruption cost maintained incrementally on StateNode.
func computeNodePoolTotals(_ context.Context, allCandidates []*Candidate, allNodes []*state.StateNode, clusterCost *cost.ClusterCost) map[string]NodePoolTotals {
	// Build candidate lookup by node name for O(1) access.
	candidateByName := make(map[string]*Candidate, len(allCandidates))
	for _, c := range allCandidates {
		candidateByName[c.Name()] = c
	}

	// First pass over candidates: collect NodePool references and fallback cost.
	type poolAccum struct {
		nodePool          *v1.NodePool
		totalCostFallback float64
	}
	accum := map[string]*poolAccum{}
	for _, c := range allCandidates {
		name := c.NodePool.Name
		a, ok := accum[name]
		if !ok {
			a = &poolAccum{nodePool: c.NodePool}
			accum[name] = a
		}
		a.totalCostFallback += c.Price
	}

	// Second pass over ALL nodes: sum disruption cost per pool.
	// Per the RFC, nodepool_total_disruption_cost = sum(node.disruption_cost for node in nodepool.nodes)
	// where node.disruption_cost = 1.0 + sum(max(0, EvictionCost(pod)) for pod in node.pods).
	disruptionByPool := map[string]float64{}
	for _, n := range allNodes {
		poolName, ok := n.Labels()[v1.NodePoolLabelKey]
		if !ok {
			continue
		}
		// Use candidate's precomputed cost when available (accurate, from pod objects).
		// For non-candidate nodes, use the incrementally-maintained exact disruption cost.
		if c, found := candidateByName[n.Name()]; found {
			disruptionByPool[poolName] += c.RescheduleDisruptionCost
		} else {
			disruptionByPool[poolName] += n.DisruptionCost()
		}
	}

	totalsMap := make(map[string]NodePoolTotals, len(accum))
	for name, a := range accum {
		totalCost := a.totalCostFallback
		if clusterCost != nil {
			if cc := clusterCost.GetNodepoolCost(a.nodePool); cc > 0 {
				totalCost = cc
			}
		}
		totalsMap[name] = NodePoolTotals{
			TotalCost:           totalCost,
			TotalDisruptionCost: disruptionByPool[name],
		}
	}
	return totalsMap
}

// ScoreMove scores a consolidation move. Approved when
// (savings/total_cost) / (disruption/total_disruption) >= 1/k.
// Caller guarantees totals come from computeNodePoolTotals, so
// TotalDisruptionCost >= PerNodeBaseDisruptionCost for any pool with candidates.
func ScoreMove(savings float64, disruptionCost float64, totals NodePoolTotals, k int32) ScoreResult {
	// Zero totals: nothing to normalize against
	if totals.TotalCost <= 0 || totals.TotalDisruptionCost <= 0 {
		return ScoreResult{K: k}
	}

	savingsFraction := savings / totals.TotalCost
	disruptionFraction := disruptionCost / totals.TotalDisruptionCost
	return ScoreResult{
		SavingsFraction:    savingsFraction,
		DisruptionFraction: disruptionFraction,
		K:                  k,
	}
}

// ComputeMoveDisruptionCost sums RescheduleDisruptionCost across candidates.
func ComputeMoveDisruptionCost(candidates []*Candidate) float64 {
	return lo.SumBy(candidates, func(c *Candidate) float64 { return c.RescheduleDisruptionCost })
}

// EvaluateBalancedMove scores each Balanced pool independently. Approved only
// when every Balanced pool approves. Non-Balanced pools are skipped.
func EvaluateBalancedMove(ctx context.Context, cmd Command, nodePoolTotals map[string]NodePoolTotals) (bool, map[string]ScoreResult) {
	if len(cmd.Candidates) == 0 {
		return false, nil
	}

	byPool := lo.GroupBy(cmd.Candidates, func(c *Candidate) string { return c.NodePool.Name })
	savings := cmd.EstimatedSavings()

	allApproved := true
	perPool := make(map[string]ScoreResult, len(byPool))

	for poolName, poolCandidates := range byPool {
		nodePool := poolCandidates[0].NodePool
		if !nodePool.Spec.Disruption.ConsolidationPolicy.IsBalanced() {
			continue
		}
		disruptionCost := cmd.PoolDisruptionCost(poolName)
		totals := nodePoolTotals[poolName]

		// For cross-pool moves, attribute net savings proportionally to each
		// pool's share of source cost. EstimatedSavings already subtracts
		// replacement cost, so this splits the net benefit by source contribution.
		poolCost := sumCandidatePrices(poolCandidates)
		totalCost := cmd.SourceCost()
		poolSavings := savings
		if totalCost > 0 && len(byPool) > 1 {
			poolSavings = savings * (poolCost / totalCost)
		}

		result := ScoreMove(poolSavings, disruptionCost, totals, v1.BalancedK)
		perPool[poolName] = result

		log.FromContext(ctx).V(1).Info("consolidation score",
			"nodepool", poolName,
			"score", result.Score(),
			"savings_fraction", result.SavingsFraction,
			"disruption_fraction", result.DisruptionFraction,
			"savings", poolSavings,
			"disruption_cost", disruptionCost,
			"nodepool_total_cost", totals.TotalCost,
			"nodepool_total_disruption_cost", totals.TotalDisruptionCost,
			"threshold", result.Threshold(),
			"k", v1.BalancedK,
			"approved", result.Approved(),
			"decision", cmd.Decision(),
			"candidates", lo.Map(poolCandidates, func(c *Candidate, _ int) string { return c.Name() }),
		)

		allApproved = allApproved && result.Approved()
	}

	return allApproved, perPool
}

// sumCandidatePrices sums Price across a set of candidates.
func sumCandidatePrices(candidates []*Candidate) float64 {
	return lo.SumBy(candidates, func(c *Candidate) float64 { return c.Price })
}

// Evaluator scores consolidation moves and decides whether they pass.
type Evaluator interface {
	ApproveCommand(ctx context.Context, cmd Command) (bool, map[string]ScoreResult)
	CanPassThreshold(c *Candidate) bool
	EmitMultiNodeEvents(ctx context.Context, cmd Command, perPool map[string]ScoreResult, approved bool)
}

// noopEvaluator always approves. Used when no balanced scoring is configured.
type noopEvaluator struct{}

func (noopEvaluator) ApproveCommand(_ context.Context, _ Command) (bool, map[string]ScoreResult) {
	return true, nil
}
func (noopEvaluator) CanPassThreshold(_ *Candidate) bool { return true }
func (noopEvaluator) EmitMultiNodeEvents(_ context.Context, _ Command, _ map[string]ScoreResult, _ bool) {
}

// balancedEvaluator wraps precomputed NodePool totals and provides a single
// ApproveCommand entry point for balanced scoring in both consolidation paths.
type balancedEvaluator struct {
	totals   map[string]NodePoolTotals
	recorder events.Recorder
}

// NewBalancedEvaluator creates an evaluator from precomputed totals.
func NewBalancedEvaluator(totals map[string]NodePoolTotals, recorder events.Recorder) Evaluator {
	return &balancedEvaluator{totals: totals, recorder: recorder}
}

// ApproveCommand scores a move. Rejections emit metrics only (not events)
// because rejection volume under Balanced is high enough to bury approvals.
func (e *balancedEvaluator) ApproveCommand(ctx context.Context, cmd Command) (bool, map[string]ScoreResult) {
	allApproved, perPool := EvaluateBalancedMove(ctx, cmd, e.totals)

	// Single-candidate: count every consolidation move. Only Balanced pools
	// emit a score histogram and ConsolidationApproved event.
	if len(cmd.Candidates) == 1 {
		candidate := cmd.Candidates[0]
		poolName := candidate.NodePool.Name
		policy := string(candidate.NodePool.Spec.Disruption.ConsolidationPolicy)
		result, scored := perPool[poolName]

		decisionLabel := "approved"
		if scored && !result.Approved() {
			decisionLabel = "rejected"
		}
		ConsolidationMovesTotal.Inc(map[string]string{"decision": decisionLabel, "nodepool": poolName, "policy": policy})

		if scored {
			ConsolidationScoreHistogram.Observe(result.Score(), map[string]string{"decision": decisionLabel, "nodepool": poolName, "policy": policy})
			if result.Approved() {
				e.recorder.Publish(disruptionevents.ConsolidationApproved(
					candidate.Node, candidate.NodeClaim,
					result.Score(), result.Threshold(), result.K,
					result.SavingsFraction*100, result.DisruptionFraction*100,
				)...)
			}
		}
	}

	return allApproved, perPool
}

func (e *balancedEvaluator) EmitMultiNodeEvents(ctx context.Context, cmd Command, perPoolResults map[string]ScoreResult, approved bool) {
	byPool := lo.GroupBy(cmd.Candidates, func(c *Candidate) string { return c.NodePool.Name })

	moveDecision := "approved"
	if !approved {
		moveDecision = "rejected"
	}

	for poolName, poolCandidates := range byPool {
		nodePool := poolCandidates[0].NodePool
		policy := string(nodePool.Spec.Disruption.ConsolidationPolicy)
		ConsolidationMovesTotal.Inc(map[string]string{"decision": moveDecision, "nodepool": poolName, "policy": policy})

		result, scored := perPoolResults[poolName]
		if !scored {
			continue
		}
		poolDecision := "approved"
		if !result.Approved() {
			poolDecision = "rejected"
		}
		ConsolidationScoreHistogram.Observe(result.Score(), map[string]string{"decision": poolDecision, "nodepool": poolName, "policy": policy})

		if result.Approved() {
			e.recorder.Publish(disruptionevents.ConsolidationApprovedMultiNode(
				nodePool,
				result.Score(), result.Threshold(), result.K,
				result.SavingsFraction*100, result.DisruptionFraction*100,
			))
		}
	}
}

// CanPassThreshold returns true if a candidate's best-case score (a full
// DELETE) meets the balanced threshold. Non-Balanced pools always pass.
func (e *balancedEvaluator) CanPassThreshold(c *Candidate) bool {
	if !c.NodePool.Spec.Disruption.ConsolidationPolicy.IsBalanced() {
		return true
	}
	totals, ok := e.totals[c.NodePool.Name]
	if !ok || totals.TotalCost <= 0 {
		return true
	}
	// A DELETE saves the full node cost with zero replacement cost — the upper
	// bound on any move's score. If even DELETE can't pass, no REPLACE will.
	result := ScoreMove(c.Price, c.RescheduleDisruptionCost, totals, v1.BalancedK)
	return result.Approved()
}
