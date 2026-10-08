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
	"sort"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	// repairUnhealthyThreshold stops repair for a NodePool when a correlated failure makes more than this fraction of
	// its nodes unhealthy. Disruption budgets continue to pace concurrent repairs below this safety threshold.
	repairUnhealthyThreshold = "20%"
)

// Repair is a voluntary disruption method that remediates unhealthy nodes. It replaces the standalone node.health
// controller: repair rides the shared disruption budget (reason "Unhealthy"), verifies rescheduling capacity before
// terminating workload-bearing nodes, orders candidates by rank + age/τ, and is vetoed by do-not-repair.
type Repair struct {
	consolidation
	policyMatcher *health.RepairPolicyMatcher
	rebootHistory *RebootHistory
}

// NewRepair constructs the repair method around the matcher cluster state matches Nodes with. It panics when cluster
// state has none, since health.NewRepairPolicyMatcher only returns nil when node repair is disabled.
func NewRepair(c consolidation) *Repair {
	policyMatcher := c.cluster.RepairPolicyMatcher()
	if policyMatcher == nil {
		panic("node repair requires cluster state built with a repair policy matcher")
	}
	return &Repair{
		consolidation: c,
		policyMatcher: policyMatcher,
		rebootHistory: newRebootHistory(c.clock),
	}
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
	// Cluster state matches the Node against the repair policies as it changes; only the toleration is resolved here.
	c.RepairPolicyResult = c.GetRepairResult(r.clock.Now())
	// Resolve rejects an empty Action, so this also skips healthy Nodes and those still within toleration.
	if !r.rebootHistory.Resolve(c) {
		return false
	}
	if c.hasPodBlockers && c.RepairPolicyResult.TerminationGracePeriod == nil && c.NodeClaim.Spec.TerminationGracePeriod == nil {
		r.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim,
			"repair requires a termination grace period to bypass blocking pods")...)
		return false
	}
	return true
}

// ComputeCommands orders eligible candidates by the repair score and returns one command for the highest-scoring
// candidate whose NodePool has budget. Workload-bearing candidates verify rescheduling capacity and pre-spin any
// required replacement; empty candidates may produce a delete-only command. Only one command per pass, mirroring drift.
//
// It logs the resolved repair decision for each candidate it acts on, which records why that policy and drain bound won.
func (r *Repair) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	cmds, err := r.computeCommands(ctx, disruptionBudgetMapping, candidates...)
	for _, cmd := range cmds {
		for _, c := range cmd.Candidates {
			log.FromContext(ctx).WithValues(append([]any{"Node", klog.KObj(c.Node)}, c.RepairPolicyResult.LogValues()...)...).Info("selected repair policy")
		}
	}
	return cmds, err
}

//nolint:gocyclo // Static and dynamic replacement flows are intentionally kept inline.
func (r *Repair) computeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	sort.SliceStable(candidates, func(i, j int) bool {
		si, sj := candidates[i].RepairPolicyResult.Score, candidates[j].RepairPolicyResult.Score
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
		if trippedPools[candidate.NodePool.Name] {
			r.recorder.Publish(disruptionevents.NodeRepairBlocked(candidate.Node, candidate.NodeClaim, candidate.NodePool,
				fmt.Sprintf("more than %s of nodes in nodepool %q are unhealthy", repairUnhealthyThreshold, candidate.NodePool.Name))...)
			continue
		}
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			continue
		}
		// Repair admits nodes with blocking (PDB / do-not-disrupt) pods only on the promise of this drain bound, so it
		// must be stamped before either replacement path returns a command.
		candidate.TerminationGracePeriod = effectiveDrainBound(candidate, candidate.RepairPolicyResult)
		// Reboot is in-place, so skip replacement and termination paths.
		if candidate.RepairPolicyResult.Action == cloudprovider.RebootNode {
			committed, err := r.commitReboot(ctx, candidate)
			if err != nil {
				return []Command{}, err
			}
			if !committed {
				continue
			}
			return []Command{{
				Candidates:          []*Candidate{candidate},
				PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
				Reboot:              true,
			}}, nil
		}
		terminateFirstEnabled := options.FromContext(ctx).FeatureGates.TerminateFirstRepair

		// Static NodePools aren't reactively scheduled, so repair can't simulate a replacement — it mirrors StaticDrift.
		if candidate.OwnedByStaticNodePool() {
			np := candidate.NodePool
			active, _, pendingDisruption := r.cluster.NodePoolState.GetNodeCount(np.Name)

			// Skip until scale-down completes: a replacement would just be surplus the deprovisioner deletes.
			if int64(active+pendingDisruption) > lo.FromPtr(np.Spec.Replicas) {
				continue
			}

			limit, ok := np.Spec.Limits[resources.Node]
			nodeLimit := lo.Ternary(ok, limit.Value(), int64(math.MaxInt64))
			// Atomic accounting, not a naive count: deleting/already-reserved nodes would otherwise let repair burst
			// past limits.nodes when commands race. A zero result reserves nothing, so the delete-only path leaks no
			// reservation; a non-zero result's slot is consumed by the replacement staged below.
			if r.cluster.NodePoolState.ReserveNodeCount(np.Name, nodeLimit, 1) == 0 {
				// Static provisioning refuses NotReady or deleting NodePools, so terminating first there would strand
				// the workload with no replacement.
				refillable := np.StatusConditions().Root().IsTrue() && np.DeletionTimestamp.IsZero()
				if !terminateFirstEnabled || !refillable {
					r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, "static NodePool is at its node limit and cannot stage a replacement")...)
					continue
				}
				return []Command{{
					Candidates:          []*Candidate{candidate},
					PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
					TerminateFirst:      true,
				}}, nil
			}
			nct := pscheduling.NewNodeClaimTemplate(np)
			result := pscheduling.Results{NewNodeClaims: []*pscheduling.NodeClaim{{NodeClaimTemplate: *nct}}}
			return []Command{{
				Candidates:          []*Candidate{candidate},
				Replacements:        replacementsFromNodeClaims(result.NewNodeClaims...),
				Results:             result,
				PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
			}}, nil
		}
		// Repair pre-spins for all reschedulable workload, including pods whose eviction is currently blocked.
		results, terminateFirst, err := SimulateSchedulingWithReservedFallback(ctx, r.kubeClient, r.cluster, r.provisioner, r.clock, r.recorder, candidate,
			terminateFirstEnabled, SimulationOptions{IncludeBlockedCandidatePods: true})
		if err != nil {
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return []Command{}, err
		}
		if terminateFirst {
			// Carry the Results (no Replacements) so nodes that can absorb the freed pods get nominated.
			return []Command{{
				Candidates:          []*Candidate{candidate},
				Results:             results,
				PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
				TerminateFirst:      true,
			}}, nil
		}
		if !results.AllNonPendingPodsScheduled() {
			r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
			continue
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

// commitReboot hands the candidate to the reboot controller and returns false if it is already rebooting or deleting.
func (r *Repair) commitReboot(ctx context.Context, candidate *Candidate) (bool, error) {
	// Re-read in case cluster state lags the API server.
	nodeClaim := &v1.NodeClaim{}
	if err := r.kubeClient.Get(ctx, client.ObjectKeyFromObject(candidate.NodeClaim), nodeClaim); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).IsTrue() || !nodeClaim.DeletionTimestamp.IsZero() {
		return false, nil
	}
	// Fall back to the NodeClaim TGP; no TGP means an unbounded drain.
	tgp := candidate.TerminationGracePeriod
	if tgp == nil && nodeClaim.Spec.TerminationGracePeriod != nil {
		tgp = &nodeClaim.Spec.TerminationGracePeriod.Duration
	}
	stored := nodeClaim.DeepCopy()
	if tgp != nil {
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
			v1.RebootTerminationGracePeriodAnnotationKey: tgp.String(),
		})
	} else {
		// A previous reboot may have left a bound behind.
		delete(nodeClaim.Annotations, v1.RebootTerminationGracePeriodAnnotationKey)
	}
	if !equality.Semantic.DeepEqual(stored.Annotations, nodeClaim.Annotations) {
		if err := r.kubeClient.Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	stored = nodeClaim.DeepCopy()
	// Attribute reboot drain evictions to repair.
	nodeClaim.StatusConditions(status.WithClock(r.clock)).SetTrueWithReason(v1.ConditionTypeDisruptionReason, string(r.Reason()), string(r.Reason()))
	nodeClaim.StatusConditions(status.WithClock(r.clock)).SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonRequested,
		fmt.Sprintf("rebooting for %s/%s", candidate.RepairPolicyResult.Condition, candidate.RepairPolicyResult.Reason))
	if err := r.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	// Make the reboot visible before the informer catches up.
	r.cluster.UpdateNodeClaim(nodeClaim)
	r.rebootHistory.RecordCommittedReboot(nodeClaim.UID)
	log.FromContext(ctx).WithValues("NodeClaim", klog.KObj(nodeClaim)).Info("committed node reboot")
	return true, nil
}

// breakerTrippedPools returns the NodePools whose unhealthy-node fraction exceeds repairUnhealthyThreshold. A node
// counts as unhealthy as soon as one of its current conditions matches the provider policy set, regardless of policy
// toleration. The threshold rounds up so one unhealthy node does not halt repair in small pools.
func (r *Repair) breakerTrippedPools(ctx context.Context) (map[string]bool, error) {
	// TODO: cache unhealthy node counts by NodePool from Node updates instead of recalculating them on every repair pass.
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
		if lo.SomeBy(node.Status.Conditions, r.policyMatcher.Matches) {
			unhealthy[nodePool]++
		}
	}
	tripped := map[string]bool{}
	thresholdValue := intstr.FromString(repairUnhealthyThreshold)
	for nodePool, count := range total {
		threshold := lo.Must(intstr.GetScaledValueFromIntOrPercent(&thresholdValue, count, true))
		if unhealthy[nodePool] > threshold {
			tripped[nodePool] = true
		}
	}
	return tripped, nil
}

// effectiveDrainBound returns the drain bound for the candidate, carried on the Command and applied by the queue
// immediately before requesting deletion: min(matched policy TGP, NodeClaim TGP), or 0 for a forceful policy. nil
// means the policy sets no bound, so the NodeClaim's own TerminationGracePeriod is inherited (the default disruption
// behavior).
// TODO: the termination-timestamp deadline is a stopgap — replace once the termination flow has a formal contract
// (kubernetes-sigs/karpenter#3029, Formalize Node Termination Contract).
func effectiveDrainBound(c *Candidate, result health.RepairResult) *time.Duration {
	if result.TerminationGracePeriod == nil {
		return nil // inherit the NodeClaim's own TerminationGracePeriod
	}
	effective := *result.TerminationGracePeriod
	if ncTGP := c.NodeClaim.Spec.TerminationGracePeriod; ncTGP != nil && ncTGP.Duration < effective {
		effective = ncTGP.Duration
	}
	return &effective
}

func (r *Repair) Reason() v1.DisruptionReason { return v1.DisruptionReasonUnhealthy }

func (r *Repair) Class() string { return RepairDisruptionClass }

func (r *Repair) ConsolidationType() string { return "" }
