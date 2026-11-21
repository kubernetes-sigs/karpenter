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

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// Repair is a voluntary disruption method that remediates unhealthy nodes. It replaces the standalone node.health
// controller: repair rides the shared disruption budget (reason "Unhealthy"), verifies rescheduling capacity before
// terminating workload-bearing nodes, orders candidates by rank + age/τ, and is vetoed by do-not-repair.
type Repair struct {
	consolidation
	policyMatcher *health.RepairPolicyMatcher
	rebootHistory *RebootHistory
	breakerLog    *health.BreakerLog
	// blocked holds why repair held back each node this pass, by provider ID. Only the disruption controller's Reconcile
	// touches it, one run at a time, so it needs no lock.
	blocked map[string]blockedNode
	// blockedStore remembers the series that repair set, so a later pass can delete the ones it no longer sets.
	blockedStore *metrics.Store
}

var _ passObserver = (*Repair)(nil)

type blockedNode struct {
	nodePool string
	reason   string
}

// NewRepair constructs the repair method around the matcher cluster state matches Nodes with. It panics when cluster
// state has none, since health.NewRepairPolicyMatcher only returns nil when node repair is disabled or replaced by --legacy-node-repair.
func NewRepair(c consolidation) *Repair {
	policyMatcher := c.cluster.RepairPolicyMatcher()
	if policyMatcher == nil {
		panic("node repair requires cluster state built with a repair policy matcher")
	}
	return &Repair{
		consolidation: c,
		policyMatcher: policyMatcher,
		rebootHistory: newRebootHistory(c.clock),
		breakerLog:    health.NewBreakerLog(c.clock),
		blocked:       map[string]blockedNode{},
		blockedStore:  metrics.NewStore(),
	}
}

// ShouldDisrupt is a predicate that filters candidates to nodes that have an unhealthy condition matching a
// RepairPolicy, have waited past that policy's toleration, and are not vetoed by the do-not-repair annotation. A vetoed
// node that repair would otherwise act on (a RepairPolicy is eligible) is reported with a Blocked event.
func (r *Repair) ShouldDisrupt(ctx context.Context, c *Candidate) bool {
	// Repair is behind the NodeRepair feature gate, and --legacy-node-repair replaces it with the node.health controller.
	if !options.FromContext(ctx).FeatureGates.NodeRepair || options.FromContext(ctx).LegacyNodeRepair {
		return false
	}
	// A disruption candidate always has a registered Node; a nil here is an invariant violation, so fail loud.
	if c.Node == nil {
		panic(fmt.Sprintf("repair candidate has no Node: %#v", c))
	}
	// Cluster state matches the Node against the repair policies as it changes; only the toleration is resolved here.
	c.RepairPolicyResult = c.GetRepairResult(r.clock.Now())
	// Resolve rejects an empty Action, so this also skips healthy Nodes and those still within toleration.
	if !r.rebootHistory.Resolve(c) {
		return false
	}
	// do-not-repair is the operator's escape hatch: it blocks all repair on this node, whatever the drain bound. It's
	// checked once a policy is eligible (its toleration has elapsed), so the veto is reported only from then on, and
	// ahead of the drain-bound check, which it overrides.
	// TODO: revisit whether do-not-disrupt should also imply do-not-repair (kubernetes-sigs/karpenter#2424), here and in
	// RepairPastDueSince.
	if c.Annotations()[v1.DoNotRepairAnnotationKey] == "true" {
		r.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim,
			fmt.Sprintf("repair is blocked through the %q annotation", v1.DoNotRepairAnnotationKey))...)
		r.block(c, RepairBlockedDoNotRepair)
		return false
	}
	if c.hasPodBlockers && repairDrainBound(c.NodeClaim, c.RepairPolicyResult) == nil {
		r.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim,
			"repair requires a termination grace period to bypass blocking pods")...)
		r.block(c, RepairBlockedBlockingPods)
		return false
	}
	return true
}

// RepairPastDueSince returns cluster state's repair result for the node and when the node became past due, or false if
// it isn't past due. A node is past due once the node registration timeout and repair's drain bound have passed since
// its earliest policy became eligible.
func RepairPastDueSince(n *state.StateNode, now time.Time) (health.RepairResult, time.Time, bool) {
	// Repair only acts on managed nodes with a NodePool, and do-not-repair vetoes it.
	if !n.Managed() || n.Labels()[v1.NodePoolLabelKey] == "" || n.Annotations()[v1.DoNotRepairAnnotationKey] == "true" {
		return health.RepairResult{}, time.Time{}, false
	}
	result := n.GetRepairResult(now)
	// A reboot in progress is repair acting, unless the NodeClaim is being deleted, as after a failed reboot.
	if result.Action == "" || (n.RebootInProgress() && !n.Deleted()) {
		return health.RepairResult{}, time.Time{}, false
	}
	since := repairDueAt(n, result.FirstEligibleAt)
	if now.Before(since) {
		return health.RepairResult{}, time.Time{}, false
	}
	return result, since, true
}

// RepairPastDueReason returns why repair hasn't finished with a past-due node. heldBack is why repair's latest pass held
// the node back, or empty if it didn't.
func RepairPastDueReason(n *state.StateNode, heldBack string) string {
	// Deleting and queued nodes come first: repair skips them, so any reason it gave one is from before then.
	switch {
	case n.Deleted():
		return RepairBlockedDeleting.Name
	// The disruption queue marks a node from when it takes the node until it deletes it.
	case n.MarkedForDeletion():
		return RepairBlockedReplacing.Name
	// A node with do-not-repair is never past due, so that reason can only be from before the annotation was removed.
	case heldBack != "" && heldBack != RepairBlockedDoNotRepair.Name:
		return heldBack
	default:
		return RepairBlockedUnknown.Name
	}
}

// repairDueAt returns when repair should have finished with the node: firstEligibleAt, when its earliest policy became
// eligible, plus the node registration timeout to launch a replacement, plus its drain bound, if any.
func repairDueAt(n *state.StateNode, firstEligibleAt time.Time) time.Time {
	// Use the drain bound as of firstEligibleAt, so a policy that becomes eligible later can't move the deadline. An
	// unbounded drain adds no time, since repair can't say when it would end.
	drainBound := repairDrainBound(n.NodeClaim, n.GetRepairResult(firstEligibleAt))
	// Add each one separately, since their sum can overflow for a huge drain bound.
	return firstEligibleAt.Add(lifecycle.RegistrationTimeout).Add(lo.FromPtr(drainBound))
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

func (r *Repair) computeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	// We use a fixed value for now so all candidates get the same value
	// This is important for repair since it scores candidates based on their current age (calculated as now - eligibleAt).
	// We don't want the order in which candidates are evaluated to affect their age.
	now := r.clock.Now()
	sort.SliceStable(candidates, func(i, j int) bool {
		si, sj := candidates[i].RepairPolicyResult.ScoreAt(now), candidates[j].RepairPolicyResult.ScoreAt(now)
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
	// Breaker and budget blocks are checked for every candidate up front, so every node they hold back gets its reason,
	// even though the loop below stops at the first candidate it acts on.
	var actionable []*Candidate
	for _, candidate := range candidates {
		if breaker, tripped := trippedPools[candidate.NodePool.Name]; tripped {
			r.breakerLog.Blocked(ctx, candidate.NodePool.Name, breaker)
			r.recorder.Publish(disruptionevents.NodeRepairBlocked(candidate.Node, candidate.NodeClaim, candidate.NodePool,
				fmt.Sprintf("more than %s of nodes in nodepool %q are unhealthy", health.UnhealthyThreshold, candidate.NodePool.Name))...)
			r.block(candidate, RepairBlockedBreaker)
			continue
		}
		// Budgets only count initialized nodes, since an uninitialized node runs no workload to protect, so repairing one
		// neither needs nor consumes budget.
		if candidate.Initialized() && disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			r.block(candidate, RepairBlockedBudget)
			continue
		}
		actionable = append(actionable, candidate)
	}
	for _, candidate := range actionable {
		// Repair admits nodes with blocking (PDB / do-not-disrupt) pods only on the promise of this drain bound, so it
		// must be stamped before either replacement path returns a command.
		candidate.TerminationGracePeriod = effectiveDrainBound(candidate.NodeClaim, candidate.RepairPolicyResult)
		// Reboot is in-place, so skip replacement and termination paths.
		if candidate.RepairPolicyResult.Action == cloudprovider.RebootNode {
			committed, err := r.commitReboot(ctx, candidate)
			if err != nil {
				return []Command{}, serrors.Wrap(fmt.Errorf("committing reboot, %w", err), "Node", klog.KObj(candidate.Node), "NodeClaim", klog.KObj(candidate.NodeClaim))
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

			nct := pscheduling.NewNodeClaimTemplate(np)
			// Only a candidate holding a slot in a still-offered reservation frees a slot the refill can use, so only it can
			// terminate first on full reservations; any other candidate can't be replaced either way, so it's blocked.
			// Checked only with terminate-first enabled, since without it there's nothing to do but replace first.
			var reservationsFull bool
			if terminateFirstEnabled {
				full, offered, err := staticReservations(ctx, r.cloudProvider, np, nct)
				// Other candidates don't depend on this one, so one NodePool's failure doesn't stop repair for the rest.
				if err != nil {
					log.FromContext(ctx).WithValues("Node", klog.KObj(candidate.Node)).Error(err, "skipping repair candidate, failed checking capacity reservations")
					continue
				}
				if full && !holdsOfferedReservation(candidate, offered) {
					r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, "static NodePool's capacity reservations are full and cannot stage a replacement")...)
					r.block(candidate, RepairBlockedNoReplacement)
					continue
				}
				reservationsFull = full
			}
			limit, ok := np.Spec.Limits[resources.Node]
			nodeLimit := lo.Ternary(ok, limit.Value(), int64(math.MaxInt64))
			// Atomic accounting, not a naive count: deleting/already-reserved nodes would otherwise let repair burst
			// past limits.nodes when commands race. A zero result reserves nothing, so the delete-only path leaks no
			// reservation; a non-zero result's slot is consumed by the replacement staged below. Full reservations are
			// checked first so the delete-only path never takes a slot.
			if reservationsFull || r.cluster.NodePoolState.ReserveNodeCount(np.Name, nodeLimit, 1) == 0 {
				// Static provisioning refuses NotReady or deleting NodePools, so terminating first there would strand
				// the workload with no replacement.
				refillable := np.StatusConditions().Root().IsTrue() && np.DeletionTimestamp.IsZero()
				if !terminateFirstEnabled || !refillable {
					r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, lo.Ternary(reservationsFull,
						"static NodePool's capacity reservations are full and cannot stage a replacement",
						"static NodePool is at its node limit and cannot stage a replacement"))...)
					r.block(candidate, RepairBlockedNoReplacement)
					continue
				}
				return []Command{{
					Candidates:           []*Candidate{candidate},
					PoolDisruptionCosts:  computePoolDisruptionCosts([]*Candidate{candidate}),
					TerminateFirstReason: lo.Ternary(reservationsFull, TerminateFirstNoReservedCapacity, TerminateFirstStaticAtLimit),
				}}, nil
			}
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
			return []Command{}, serrors.Wrap(fmt.Errorf("simulating scheduling, %w", err), "Node", klog.KObj(candidate.Node), "NodeClaim", klog.KObj(candidate.NodeClaim))
		}
		if terminateFirst {
			// Carry the Results (no Replacements) so nodes that can absorb the freed pods get nominated.
			return []Command{{
				Candidates:           []*Candidate{candidate},
				Results:              results,
				PoolDisruptionCosts:  computePoolDisruptionCosts([]*Candidate{candidate}),
				TerminateFirstReason: TerminateFirstNoReservedCapacity,
			}}, nil
		}
		if !results.AllNonPendingPodsScheduled() {
			r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
			r.block(candidate, lo.Ternary(waitingOnUninitializedNodes(results), RepairBlockedWaitingOnInitialization, RepairBlockedNoReplacement))
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
	tgp := repairDrainBound(nodeClaim, candidate.RepairPolicyResult)
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

// breakerTrippedPools returns the NodePools whose unhealthy-node fraction exceeds health.UnhealthyThreshold. A node
// counts as unhealthy as soon as one of its current conditions matches the provider policy set, regardless of policy
// toleration. The threshold rounds up so one unhealthy node does not halt repair in small pools.
func (r *Repair) breakerTrippedPools(ctx context.Context) (map[string]health.Breaker, error) {
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
	tripped := map[string]health.Breaker{}
	for nodePool, count := range total {
		if breaker := health.CheckBreaker(count, unhealthy[nodePool]); breaker.Tripped {
			tripped[nodePool] = breaker
		}
	}
	return tripped, nil
}

// waitingOnUninitializedNodes reports whether every pod repair must move fails only because it would land on a node
// that hasn't initialized, so repair is waiting for that node rather than lacking room.
func waitingOnUninitializedNodes(results pscheduling.Results) bool {
	errs := results.NonPendingPodErrors()
	return len(errs) > 0 && lo.EveryBy(lo.Values(errs), func(err error) bool {
		_, ok := lo.ErrorsAs[*UninitializedNodeError](err)
		return ok
	})
}

// block records why repair held back the candidate this pass.
func (r *Repair) block(c *Candidate, reason opmetrics.Value) {
	r.blocked[c.ProviderID()] = blockedNode{nodePool: c.NodePool.Name, reason: reason.Name}
}

// startPass clears the previous pass's nodes, so only nodes held back this pass have a reason.
func (r *Repair) startPass() {
	r.blocked = map[string]blockedNode{}
}

// endPass saves why repair held back each node this pass in cluster state, and publishes how many nodes it held back
// by NodePool and reason. It doesn't clear them, since the controller calls it again when the Reconcile returns.
func (r *Repair) endPass() {
	r.cluster.UpdateRepairBlockedReasons(lo.MapValues(r.blocked, func(b blockedNode, _ string) string { return b.reason }))
	storeMetrics := map[string][]*metrics.StoreMetric{}
	for b, count := range lo.CountValues(lo.Values(r.blocked)) {
		storeMetrics[b.nodePool] = append(storeMetrics[b.nodePool], &metrics.StoreMetric{
			GaugeMetric: NodeClaimsUnhealthyBlocked,
			Value:       float64(count),
			Labels:      map[string]string{metrics.NodePoolLabel: b.nodePool, repairBlockedReasonLabel: b.reason},
		})
	}
	// The store compares every old series with every new one under the same key, so one key per NodePool keeps that
	// cheap. ReplaceAll also deletes the series of NodePools with no blocked nodes this pass.
	r.blockedStore.ReplaceAll(storeMetrics)
}

// effectiveDrainBound returns the drain bound for the candidate's NodeClaim, carried on the Command and applied by the
// queue immediately before requesting deletion: min(matched policy TGP, NodeClaim TGP), or 0 for a forceful policy.
// nil means the policy sets no bound, so the NodeClaim's own TerminationGracePeriod is inherited (the default
// disruption behavior).
// TODO: the termination-timestamp deadline is a stopgap — replace once the termination flow has a formal contract
// (kubernetes-sigs/karpenter#3029, Formalize Node Termination Contract).
func effectiveDrainBound(nodeClaim *v1.NodeClaim, result health.RepairResult) *time.Duration {
	if result.TerminationGracePeriod == nil {
		return nil // inherit the NodeClaim's own TerminationGracePeriod
	}
	effective := *result.TerminationGracePeriod
	if ncTGP := nodeClaim.Spec.TerminationGracePeriod; ncTGP != nil && ncTGP.Duration < effective {
		effective = ncTGP.Duration
	}
	return &effective
}

// repairDrainBound returns how long repair lets the node drain, counting the NodeClaim's own bound, which
// effectiveDrainBound leaves to termination. nil means the drain is unbounded.
func repairDrainBound(nodeClaim *v1.NodeClaim, result health.RepairResult) *time.Duration {
	if bound := effectiveDrainBound(nodeClaim, result); bound != nil {
		return bound
	}
	if tgp := nodeClaim.Spec.TerminationGracePeriod; tgp != nil {
		return new(tgp.Duration)
	}
	return nil
}

func (r *Repair) Reason() v1.DisruptionReason { return v1.DisruptionReasonUnhealthy }

func (r *Repair) Class() string { return RepairDisruptionClass }

func (r *Repair) ConsolidationType() string { return "" }
