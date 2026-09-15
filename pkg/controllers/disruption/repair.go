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
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// agingConstant (τ) is the time a node must wait past its toleration to earn one rank tier of standing. It sets the
// starvation bound: a node overtakes a steadily-refreshed rival Δrank tiers up after Δrank·τ. See resiliency §3.1.2.
const agingConstant = 30 * time.Minute

// repairUnhealthyThreshold reinstates the retired node.health controller's circuit breaker: repair stops for a
// NodePool once more than this fraction of its nodes are unhealthy, so a correlated failure (bad AMI, AZ outage)
// isn't amplified by mass-replacing nodes into the same fault. This is a blunt interim backstop for correlated-failure
// restraint (kubernetes-sigs/karpenter#3031); the disruption budget still paces the concurrent repairs under it.
var repairUnhealthyThreshold = intstr.FromString("20%")

// Repair is a voluntary disruption method that remediates unhealthy nodes. It replaces the standalone node.health
// controller: repair rides the shared disruption budget (reason "Unhealthy"), pre-spins a replacement before
// terminating (replace-then-terminate), orders candidates by rank + age/τ, and is vetoed by do-not-repair.
type Repair struct {
	consolidation
	// repairPolicies and ranks are cached at construction. Provider-authored defaults are static, so re-reading them on
	// every pass (per candidate, in score/denseRanks) is wasted work; providers must not mutate them.
	repairPolicies []cloudprovider.RepairPolicy
	ranks          map[int]int // configured priority -> dense rank
}

func NewRepair(c consolidation) *Repair {
	policies := c.cloudProvider.RepairPolicies()
	return &Repair{consolidation: c, repairPolicies: policies, ranks: denseRanks(policies)}
}

// ShouldDisrupt is a predicate that filters candidates to nodes that have an unhealthy condition matching a
// RepairPolicy, have waited past that policy's toleration, and are not vetoed by the do-not-repair annotation.
func (r *Repair) ShouldDisrupt(ctx context.Context, c *Candidate) bool {
	// Repair is behind the NodeRepair feature gate, matching the old node.health controller's gating.
	if !options.FromContext(ctx).FeatureGates.NodeRepair {
		return false
	}
	// A disruption candidate always has a registered Node; a nil here is an invariant violation, so fail loud.
	if c.Node == nil {
		panic(fmt.Sprintf("repair candidate has no Node: %#v", c))
	}
	// do-not-repair is the operator's escape hatch: it blocks all repair on this node, whatever the drain bound.
	// TODO: revisit whether do-not-disrupt should also imply do-not-repair (kubernetes-sigs/karpenter#2424).
	if c.Annotations()[v1.DoNotRepairAnnotationKey] == "true" {
		return false
	}
	policy, cond := r.matchRepairPolicy(c.Node)
	if policy == nil {
		return false
	}
	// Eligibility is delayed by the policy's toleration — a confidence window before repair acts.
	return !r.clock.Now().Before(cond.LastTransitionTime.Add(policy.TolerationDuration))
}

// ComputeCommands orders eligible candidates by the repair score and returns one replace-then-terminate command for the
// highest-scoring candidate whose NodePool has budget. Only one command per pass, mirroring drift.
func (r *Repair) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	ranks := r.ranks
	sort.SliceStable(candidates, func(i, j int) bool {
		si, sj := r.score(candidates[i], ranks), r.score(candidates[j], ranks)
		if si != sj {
			return si > sj // higher score repairs first
		}
		// Deterministic tie-break: lower disruption cost, then node name, so the same set always yields the same order.
		if candidates[i].DisruptionCost != candidates[j].DisruptionCost {
			return candidates[i].DisruptionCost < candidates[j].DisruptionCost
		}
		return candidates[i].Name() < candidates[j].Name()
	})

	trippedPools, err := r.breakerTrippedPools(ctx)
	if err != nil {
		return []Command{}, err
	}
	for _, candidate := range candidates {
		// Circuit breaker: if too much of the NodePool is unhealthy, stop repairing it — the fault is likely
		// correlated (bad AMI, AZ outage) and replacing more nodes would amplify the outage, not fix it.
		if trippedPools[candidate.NodePool.Name] {
			r.recorder.Publish(disruptionevents.NodeRepairBlocked(candidate.Node, candidate.NodeClaim, candidate.NodePool,
				fmt.Sprintf("more than %s of nodes in nodepool %q are unhealthy", repairUnhealthyThreshold.String(), candidate.NodePool.Name))...)
			continue
		}
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			continue
		}
		// Pre-spin the replacement; the queue terminates the original only once the replacement is healthy.
		results, err := SimulateScheduling(ctx, r.kubeClient, r.cluster, r.provisioner, r.clock, r.recorder, nil, candidate)
		if err != nil {
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return []Command{}, err
		}
		if !results.AllNonPendingPodsScheduled() {
			r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
			continue
		}
		// Set the candidate's drain bound; the queue stamps the absolute deadline at actual deletion time (after the
		// replacement is healthy), so repair is never an unbounded hang and a forceful (0) policy skips the drain for
		// conditions the kubelet can't evict through — without pre-spin latency eroding the window.
		candidate.TerminationGracePeriod = r.effectiveDrainBound(candidate)
		// Preserve the per-condition/per-image disruption metric the retired node.health controller emitted.
		if _, cond := r.matchRepairPolicy(candidate.Node); cond != nil {
			NodeClaimsUnhealthyDisruptedTotal.Inc(map[string]string{
				conditionLabel:            pretty.ToSnakeCase(string(cond.Type)),
				metrics.NodePoolLabel:     candidate.NodePool.Name,
				metrics.CapacityTypeLabel: candidate.NodeClaim.Labels[v1.CapacityTypeLabelKey],
				imageIDLabel:              candidate.NodeClaim.Status.ImageID,
			})
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

// breakerTrippedPools returns the set of NodePool names whose unhealthy-node fraction exceeds repairUnhealthyThreshold.
// A node counts as unhealthy when it matches any RepairPolicy (the same signal repair acts on), regardless of
// toleration; the denominator is every node carrying a NodePool label. Mirrors the retired node.health breaker
// (rounding up). Reads straight from the informer cache (UnsafeDisableDeepCopy) — we only read the nodes, never mutate.
func (r *Repair) breakerTrippedPools(ctx context.Context) (map[string]bool, error) {
	nodeList := &corev1.NodeList{}
	if err := r.kubeClient.List(ctx, nodeList, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	total := map[string]int{}
	unhealthy := map[string]int{}
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		nodePool := node.Labels[v1.NodePoolLabelKey]
		if nodePool == "" {
			continue
		}
		total[nodePool]++
		if policy, _ := r.matchRepairPolicy(node); policy != nil {
			unhealthy[nodePool]++
		}
	}
	tripped := map[string]bool{}
	for nodePool, count := range total {
		threshold := lo.Must(intstr.GetScaledValueFromIntOrPercent(&repairUnhealthyThreshold, count, true))
		if unhealthy[nodePool] > threshold {
			tripped[nodePool] = true
		}
	}
	return tripped, nil
}

// score computes E = rank + age/τ for a node: the argmax of that expression over ALL of the node's matching
// conditions, not just the highest-priority one. Age is time past toleration (post-eligibility), so a flakier signal's
// longer toleration never leaks into its standing. Taking the argmax (rather than reusing matchRepairPolicy's
// priority-first pick) keeps inter-node ordering consistent: a node's importance is its most urgent condition, so a
// low-priority-but-long-starving condition still lifts the node even when a fresh high-priority condition also trips.
func (r *Repair) score(c *Candidate, ranks map[int]int) float64 {
	best := 0.0
	for i := range r.repairPolicies {
		policy := r.repairPolicies[i]
		cond := nodeutils.GetCondition(c.Node, policy.ConditionType)
		if cond.Status != policy.ConditionStatus {
			continue
		}
		age := r.clock.Now().Sub(cond.LastTransitionTime.Add(policy.TolerationDuration))
		if age < 0 {
			age = 0
		}
		best = max(best, float64(ranks[policy.Priority])+age.Minutes()/agingConstant.Minutes())
	}
	return best
}

// denseRanks compresses the set of configured policy priorities into contiguous tiers (adjacent tiers one apart),
// so arbitrary priority magnitudes can't change what τ means — only the ordering of priorities matters. Computed once
// at construction (the policy set is static) and cached on the Repair as ranks.
func denseRanks(policies []cloudprovider.RepairPolicy) map[int]int {
	priorities := lo.Uniq(lo.Map(policies, func(p cloudprovider.RepairPolicy, _ int) int { return p.Priority }))
	sort.Ints(priorities)
	ranks := make(map[int]int, len(priorities))
	for i, p := range priorities {
		ranks[p] = i // lowest priority -> rank 0, ascending
	}
	return ranks
}

// matchRepairPolicy returns the highest-priority RepairPolicy whose (type,status) matches an unhealthy condition on
// the node, plus the matched condition — the single policy that governs the repair ACTION (its drain bound). When a
// node trips multiple policies the highest priority wins, ties broken by the earlier toleration deadline. Note this is
// deliberately NOT how score orders nodes (score argmaxes rank+age across all conditions); this pick is for the action,
// score is for inter-node ordering.
// TODO: rip out for the reason-aware matching model (kubernetes-sigs/karpenter#3263, reason-aware repair policy
// matching + escalation) — picking a single highest-priority policy is a placeholder for multi-reason semantics.
func (r *Repair) matchRepairPolicy(node *corev1.Node) (*cloudprovider.RepairPolicy, *corev1.NodeCondition) {
	var best *cloudprovider.RepairPolicy
	var bestCond *corev1.NodeCondition
	deadline := time.Time{}
	for i := range r.repairPolicies {
		policy := r.repairPolicies[i]
		cond := nodeutils.GetCondition(node, policy.ConditionType)
		if cond.Status != policy.ConditionStatus {
			continue
		}
		terminationTime := cond.LastTransitionTime.Add(policy.TolerationDuration)
		if best == nil || policy.Priority > best.Priority ||
			(policy.Priority == best.Priority && terminationTime.Before(deadline)) {
			p := policy
			c := cond
			best, bestCond, deadline = &p, &c, terminationTime
		}
	}
	return best, bestCond
}

// effectiveDrainBound returns the drain bound for the candidate, carried on the Command and applied by the queue at
// deletion time: min(matched policy TGP, NodeClaim TGP), or 0 for a forceful policy. nil means the policy sets no
// bound, so the NodeClaim's own TerminationGracePeriod is inherited (the default disruption behavior).
// TODO: the termination-timestamp deadline is a stopgap — replace once the termination flow has a formal contract
// (kubernetes-sigs/karpenter#3029, Formalize Node Termination Contract).
func (r *Repair) effectiveDrainBound(c *Candidate) *time.Duration {
	policy, _ := r.matchRepairPolicy(c.Node)
	if policy == nil || policy.TerminationGracePeriod == nil {
		return nil // inherit the NodeClaim's own TerminationGracePeriod
	}
	effective := *policy.TerminationGracePeriod
	if ncTGP := c.NodeClaim.Spec.TerminationGracePeriod; ncTGP != nil && ncTGP.Duration < effective {
		effective = ncTGP.Duration
	}
	return &effective
}

func (r *Repair) Reason() v1.DisruptionReason { return v1.DisruptionReasonUnhealthy }

func (r *Repair) Class() string { return RepairDisruptionClass }

func (r *Repair) ConsolidationType() string { return "" }
