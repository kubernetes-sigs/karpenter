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
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	// agingConstant (τ) is the time a node must wait past its toleration to earn one rank tier of standing. It sets the
	// starvation bound: a node overtakes a steadily-refreshed rival Δrank tiers up after Δrank·τ. See resiliency §3.1.2.
	agingConstant = 30 * time.Minute
	// repairSimulationAttemptsPerPass bounds full-cluster scheduling simulations in one disruption pass. Candidates
	// that fail simulation are retried with process-local backoff, allowing lower-ranked candidates to make progress.
	repairSimulationAttemptsPerPass = 10
	repairSimulationBackoffBase     = time.Minute
	repairSimulationBackoffMax      = 10 * time.Minute
	repairDecisionLogRetention      = time.Hour
	repairDecisionLogPruneInterval  = 10 * time.Minute
)

// Repair is a voluntary disruption method that remediates unhealthy nodes. It replaces the standalone node.health
// controller: repair rides the shared disruption budget (reason "Unhealthy"), pre-spins a replacement before
// terminating (replace-then-terminate), orders candidates by rank + age/τ, and is vetoed by do-not-repair.
type Repair struct {
	consolidation
	ranks                map[int]int // configured priority -> dense rank
	policyMatcher        *health.RepairPolicyMatcher
	simulationRetriesMu  sync.Mutex
	simulationRetries    map[types.UID]repairSimulationRetry
	decisionLogsMu       sync.Mutex
	decisionLogs         map[types.UID]repairDecisionLogState
	nextDecisionLogPrune time.Time
}

type repairSimulationRetry struct {
	failures   int
	retryAfter time.Time
}

type repairDecisionLogState struct {
	fingerprint string
	lastSeen    time.Time
}

// NewRepair validates and compiles the provider's complete repair policy set before constructing the method.
func NewRepair(c consolidation) (*Repair, error) {
	policies := c.cloudProvider.RepairPolicies()
	policyMatcher, err := health.NewRepairPolicyMatcher(policies, sets.New(cloudprovider.ReplaceNode))
	if err != nil {
		return nil, err
	}
	return &Repair{
		consolidation:     c,
		ranks:             denseRanks(policies),
		policyMatcher:     policyMatcher,
		simulationRetries: make(map[types.UID]repairSimulationRetry),
		decisionLogs:      make(map[types.UID]repairDecisionLogState),
	}, nil
}

// ShouldConsider cheaply rejects healthy or not-yet-eligible nodes before disruption candidate construction.
func (r *Repair) ShouldConsider(ctx context.Context, node *state.StateNode) bool {
	if !options.FromContext(ctx).FeatureGates.NodeRepair ||
		node.Node == nil ||
		node.Annotations()[v1.DoNotRepairAnnotationKey] == "true" {
		return false
	}
	now := r.clock.Now()
	r.logRepairPolicyDecisions(ctx, node.Node, now)
	return lo.SomeBy(node.Node.Status.Conditions, func(condition corev1.NodeCondition) bool {
		return r.policyMatcher.Evaluate(condition, now) != nil
	})
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
	r.logRepairPolicyDecisions(ctx, c.Node, r.clock.Now())
	result, _ := r.matchRepairPolicy(c.Node)
	if result == nil {
		return false
	}
	if c.hasPodBlockers && result.TerminationGracePeriod == nil && c.NodeClaim.Spec.TerminationGracePeriod == nil {
		r.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim,
			"repair requires a termination grace period to bypass blocking pods")...)
		return false
	}
	return true
}

// ComputeCommands orders eligible candidates by the repair score and returns one replace-then-terminate command for the
// highest-scoring candidate whose NodePool has budget. Only one command per pass, mirroring drift.
func (r *Repair) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	r.sortCandidates(candidates)
	r.pruneSimulationRetries(candidates)
	now := r.clock.Now()
	simulationAttempts := 0
	for _, candidate := range candidates {
		command, ok, err := r.commandForCandidate(ctx, candidate, disruptionBudgetMapping, now, &simulationAttempts)
		if err != nil {
			return []Command{}, err
		}
		if ok {
			return []Command{command}, nil
		}
	}
	return []Command{}, nil
}

func (r *Repair) commandForCandidate(
	ctx context.Context,
	candidate *Candidate,
	disruptionBudgetMapping map[string]int,
	now time.Time,
	simulationAttempts *int,
) (Command, bool, error) {
	if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
		return Command{}, false, nil
	}
	nodeClaimUID := candidate.NodeClaim.UID
	dynamicCandidate := !candidate.OwnedByStaticNodePool()
	if dynamicCandidate && !r.allowSimulation(nodeClaimUID, now, simulationAttempts) {
		return Command{}, false, nil
	}
	candidate, results, ok, backoff, err := r.replacementForCandidate(ctx, candidate)
	if err != nil {
		return Command{}, false, err
	}
	if !ok {
		if dynamicCandidate && backoff {
			r.recordSimulationFailure(nodeClaimUID, now)
		}
		return Command{}, false, nil
	}
	if dynamicCandidate {
		r.forgetSimulationFailure(nodeClaimUID)
	}
	// Set the candidate's drain bound; the queue stamps the absolute deadline at actual deletion time (after the
	// replacement is healthy), so repair is never an unbounded hang and a forceful (0) policy skips the drain for
	// conditions the kubelet can't evict through — without pre-spin latency eroding the window.
	candidate.TerminationGracePeriod = r.effectiveDrainBound(candidate)
	if _, cond := r.matchRepairPolicy(candidate.Node); cond != nil {
		candidate.RepairCondition = cond.Type
	}
	return Command{
		Candidates:          []*Candidate{candidate},
		Replacements:        replacementsFromNodeClaims(results.NewNodeClaims...),
		Results:             results,
		PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
	}, true, nil
}

func (r *Repair) allowSimulation(nodeClaimUID types.UID, now time.Time, attempts *int) bool {
	if *attempts >= repairSimulationAttemptsPerPass {
		return false
	}
	r.simulationRetriesMu.Lock()
	defer r.simulationRetriesMu.Unlock()
	if retry, ok := r.simulationRetries[nodeClaimUID]; ok && now.Before(retry.retryAfter) {
		return false
	}
	(*attempts)++
	return true
}

func (r *Repair) recordSimulationFailure(nodeClaimUID types.UID, now time.Time) {
	r.simulationRetriesMu.Lock()
	defer r.simulationRetriesMu.Unlock()
	if r.simulationRetries == nil {
		r.simulationRetries = make(map[types.UID]repairSimulationRetry)
	}
	retry := r.simulationRetries[nodeClaimUID]
	retry.failures++
	delay := repairSimulationBackoffBase
	for i := 1; i < retry.failures && delay < repairSimulationBackoffMax; i++ {
		delay = min(delay*2, repairSimulationBackoffMax)
	}
	retry.retryAfter = now.Add(delay)
	r.simulationRetries[nodeClaimUID] = retry
}

func (r *Repair) forgetSimulationFailure(nodeClaimUID types.UID) {
	r.simulationRetriesMu.Lock()
	defer r.simulationRetriesMu.Unlock()
	delete(r.simulationRetries, nodeClaimUID)
}

func (r *Repair) pruneSimulationRetries(candidates []*Candidate) {
	active := sets.New[types.UID]()
	for _, candidate := range candidates {
		active.Insert(candidate.NodeClaim.UID)
	}
	r.simulationRetriesMu.Lock()
	defer r.simulationRetriesMu.Unlock()
	for nodeClaimUID := range r.simulationRetries {
		if !active.Has(nodeClaimUID) {
			delete(r.simulationRetries, nodeClaimUID)
		}
	}
}

func (r *Repair) sortCandidates(candidates []*Candidate) {
	ranks := r.ranks
	now := r.clock.Now()
	scores := make(map[*Candidate]float64, len(candidates))
	for _, candidate := range candidates {
		scores[candidate] = r.score(candidate, ranks, now)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		si, sj := scores[candidates[i]], scores[candidates[j]]
		if si != sj {
			return si > sj // higher score repairs first
		}
		// Deterministic tie-break: lower disruption cost, then node name, so the same set always yields the same order.
		if candidates[i].DisruptionCost != candidates[j].DisruptionCost {
			return candidates[i].DisruptionCost < candidates[j].DisruptionCost
		}
		return candidates[i].Name() < candidates[j].Name()
	})
}

func (r *Repair) NeedsNodePoolTotals() bool {
	return false
}

func (r *Repair) replacementForCandidate(ctx context.Context, candidate *Candidate) (*Candidate, pscheduling.Results, bool, bool, error) {
	if candidate.OwnedByStaticNodePool() {
		current, err := r.revalidateCandidate(ctx, candidate)
		if err != nil || current == nil {
			return nil, pscheduling.Results{}, false, false, err
		}
		results, ok := r.staticReplacement(current)
		return current, results, ok, false, nil
	}
	return r.dynamicReplacement(ctx, candidate)
}

func (r *Repair) dynamicReplacement(ctx context.Context, candidate *Candidate) (*Candidate, pscheduling.Results, bool, bool, error) {
	// Repair pre-spins for all reschedulable workload, including pods whose eviction is currently blocked.
	results, err := simulateScheduling(ctx, r.kubeClient, r.cluster, r.provisioner, r.clock, r.recorder, nil,
		simulationOptions{
			includeBlockedCandidatePods: true,
			ensureReplacementNodePool:   candidate.NodePool.Name,
			candidatePodsOnly:           len(candidate.reschedulablePods) == 0,
		},
		candidate,
	)
	if err != nil {
		if errors.Is(err, errCandidateDeleting) {
			return nil, pscheduling.Results{}, false, false, nil
		}
		if isCandidateBlockedError(err) {
			r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(err.Error()))...)
			return nil, pscheduling.Results{}, false, true, nil
		}
		return nil, pscheduling.Results{}, false, false, err
	}
	if !results.AllNonPendingPodsScheduled() {
		r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
		return nil, pscheduling.Results{}, false, true, nil
	}
	current, err := r.revalidateCandidate(ctx, candidate)
	if err != nil || current == nil {
		return nil, pscheduling.Results{}, false, false, err
	}
	// Revalidation refreshes health and PDB admission. If any input that affected the scheduling result also changed,
	// discard this pass instead of pairing fresh candidate state with stale replacement capacity.
	if !sameSchedulingInputs(candidate, current) {
		return nil, pscheduling.Results{}, false, false, nil
	}
	return current, results, true, false, nil
}

func (r *Repair) revalidateCandidate(ctx context.Context, candidate *Candidate) (*Candidate, error) {
	currentNode := r.currentCandidateNode(candidate)
	if currentNode == nil {
		return nil, nil
	}

	nodePool := &v1.NodePool{}
	if err := r.kubeClient.Get(ctx, client.ObjectKey{Name: candidate.NodePool.Name}, nodePool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting NodePool while revalidating repair candidate, %w", err)
	}
	if !nodepoolutils.IsManaged(nodePool, r.cloudProvider) {
		return nil, nil
	}
	if nodePool.UID != candidate.NodePool.UID || nodePool.Generation != candidate.NodePool.Generation {
		return nil, nil
	}
	pdbs, err := pdb.NewLimits(ctx, r.kubeClient)
	if err != nil {
		return nil, fmt.Errorf("tracking PodDisruptionBudgets while revalidating repair candidate, %w", err)
	}
	instanceTypeName := currentNode.Labels()[corev1.LabelInstanceTypeStable]
	current, err := NewCandidate(
		ctx,
		r.kubeClient,
		r.recorder,
		r.clock,
		currentNode,
		pdbs,
		map[string]*v1.NodePool{nodePool.Name: nodePool},
		map[string]map[string]*cloudprovider.InstanceType{
			nodePool.Name: {instanceTypeName: candidate.instanceType},
		},
		r.queue,
		RepairDisruptionClass,
	)
	if err != nil {
		log.FromContext(ctx).V(1).Info("discarding repair candidate after revalidation", "Node", klog.KObj(candidate.Node), "error", err)
		return nil, nil //nolint:nilerr // Candidate validation failures make this candidate stale for the current pass.
	}
	if !r.ShouldDisrupt(ctx, current) {
		return nil, nil
	}
	return current, nil
}

func (r *Repair) currentCandidateNode(candidate *Candidate) *state.StateNode {
	current, ok := r.cluster.DeepCopyNode(candidate.ProviderID())
	if !ok ||
		current.Node == nil ||
		current.NodeClaim == nil ||
		current.Node.UID != candidate.Node.UID ||
		current.NodeClaim.UID != candidate.NodeClaim.UID {
		return nil
	}
	return current
}

func sameSchedulingInputs(previous, current *Candidate) bool {
	if !apiequality.Semantic.DeepEqual(previous.NodePool.Spec, current.NodePool.Spec) ||
		!apiequality.Semantic.DeepEqual(previous.NodeClaim.Spec, current.NodeClaim.Spec) ||
		!apiequality.Semantic.DeepEqual(previous.NodeClaim.Labels, current.NodeClaim.Labels) ||
		!apiequality.Semantic.DeepEqual(previous.Node.Labels, current.Node.Labels) ||
		!apiequality.Semantic.DeepEqual(previous.Node.Spec.Taints, current.Node.Spec.Taints) ||
		!apiequality.Semantic.DeepEqual(previous.Node.Status.Allocatable, current.Node.Status.Allocatable) ||
		previous.hasPodBlockers != current.hasPodBlockers ||
		len(previous.reschedulablePods) != len(current.reschedulablePods) {
		return false
	}
	currentPods := lo.SliceToMap(current.reschedulablePods, func(pod *corev1.Pod) (types.UID, *corev1.Pod) {
		return pod.UID, pod
	})
	return lo.EveryBy(previous.reschedulablePods, func(pod *corev1.Pod) bool {
		currentPod, ok := currentPods[pod.UID]
		return ok && pod.ResourceVersion == currentPod.ResourceVersion
	})
}

func (r *Repair) staticReplacement(candidate *Candidate) (pscheduling.Results, bool) {
	nodeLimit := int64(math.MaxInt64)
	if limit, ok := candidate.NodePool.Spec.Limits[resources.Node]; ok {
		nodeLimit = limit.Value()
	}
	runningNodes, _, pendingDisruption := r.cluster.NodePoolState.GetNodeCount(candidate.NodePool.Name)
	if int64(runningNodes+pendingDisruption) > lo.FromPtr(candidate.NodePool.Spec.Replicas) {
		r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim,
			fmt.Sprintf("static NodePool %q is still scaling down", candidate.NodePool.Name))...)
		return pscheduling.Results{}, false
	}
	if r.cluster.NodePoolState.ReserveNodeCount(candidate.NodePool.Name, nodeLimit, 1) == 0 {
		r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim,
			fmt.Sprintf("static NodePool %q has no node limit available for a replacement", candidate.NodePool.Name))...)
		return pscheduling.Results{}, false
	}
	template := pscheduling.NewNodeClaimTemplate(candidate.NodePool)
	return pscheduling.Results{
		NewNodeClaims: []*pscheduling.NodeClaim{{NodeClaimTemplate: *template}},
	}, true
}

// score computes E = rank + age/τ for a node: the argmax of that expression over ALL of the node's eligible matching
// conditions, not just the highest-priority one. Age is time past toleration (post-eligibility), so a flakier signal's
// longer toleration never leaks into its standing. Taking the argmax keeps inter-node ordering consistent: a node's
// importance is its most urgent eligible condition, so a low-priority-but-long-starving condition still lifts the node
// even when a fresh high-priority condition also trips.
// TODO: re-introduce a per-NodePool backoff term (subtracted here) once the NodePool backoff implementation lands
// (kubernetes-sigs/karpenter#3178) — it was ripped out to avoid duplicating that mechanism.
func (r *Repair) score(c *Candidate, ranks map[int]int, now time.Time) float64 {
	best := 0.0
	for _, cond := range c.Node.Status.Conditions {
		for _, policy := range r.policyMatcher.EligiblePolicies(cond, now) {
			age := now.Sub(cond.LastTransitionTime.Add(policy.TolerationDuration))
			if age < 0 {
				age = 0
			}
			best = max(best, float64(ranks[policy.Priority])+age.Minutes()/agingConstant.Minutes())
		}
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

// matchRepairPolicy evaluates the reason-aware policy group for the highest-priority unhealthy condition selected by
// voluntary repair. Candidate resolution across multiple unhealthy conditions is intentionally left to the follow-up
// repair orchestration work; this preserves the existing voluntary ordering while making each condition's eligibility
// deterministic and reason-aware.
func (r *Repair) matchRepairPolicy(node *corev1.Node) (*health.RepairPolicyResult, *corev1.NodeCondition) {
	var best *cloudprovider.RepairPolicy
	var bestCond *corev1.NodeCondition
	deadline := time.Time{}
	now := r.clock.Now()
	for _, cond := range node.Status.Conditions {
		for _, policy := range r.policyMatcher.EligiblePolicies(cond, now) {
			terminationTime := cond.LastTransitionTime.Add(policy.TolerationDuration)
			if best == nil || policy.Priority > best.Priority ||
				(policy.Priority == best.Priority && terminationTime.Before(deadline)) {
				p := policy
				c := cond
				best, bestCond, deadline = &p, &c, terminationTime
			}
		}
	}
	if bestCond == nil {
		return nil, nil
	}
	return r.policyMatcher.Evaluate(*bestCond, now), bestCond
}

func (r *Repair) logRepairPolicyDecisions(ctx context.Context, node *corev1.Node, now time.Time) {
	logger := log.FromContext(ctx).V(1)
	if !logger.Enabled() {
		return
	}
	decisions := make([][]any, 0, len(node.Status.Conditions))
	var fingerprint strings.Builder
	for _, condition := range node.Status.Conditions {
		values := r.policyMatcher.DecisionLogValues(condition, now)
		if len(values) == 0 {
			continue
		}
		decisions = append(decisions, values)
		for _, value := range values {
			_, _ = fmt.Fprintf(&fingerprint, "%T=%v\x00", value, value)
		}
		fingerprint.WriteByte('\n')
	}
	if len(decisions) == 0 {
		r.clearRepairPolicyDecisionLog(node)
		return
	}
	if !r.recordRepairPolicyDecisionLog(node, fingerprint.String(), now) {
		return
	}
	for _, values := range decisions {
		logger.WithValues(append([]any{
			"Node", klog.KObj(node),
		}, values...)...).Info("evaluated repair policy")
	}
}

func (r *Repair) recordRepairPolicyDecisionLog(node *corev1.Node, fingerprint string, now time.Time) bool {
	key := node.UID
	if key == "" {
		key = types.UID(node.Name)
	}
	r.decisionLogsMu.Lock()
	defer r.decisionLogsMu.Unlock()
	if r.decisionLogs == nil {
		r.decisionLogs = make(map[types.UID]repairDecisionLogState)
	}
	if r.nextDecisionLogPrune.IsZero() || !now.Before(r.nextDecisionLogPrune) {
		cutoff := now.Add(-repairDecisionLogRetention)
		for uid, state := range r.decisionLogs {
			if state.lastSeen.Before(cutoff) {
				delete(r.decisionLogs, uid)
			}
		}
		r.nextDecisionLogPrune = now.Add(repairDecisionLogPruneInterval)
	}
	previous, ok := r.decisionLogs[key]
	r.decisionLogs[key] = repairDecisionLogState{fingerprint: fingerprint, lastSeen: now}
	return !ok || previous.fingerprint != fingerprint
}

func (r *Repair) clearRepairPolicyDecisionLog(node *corev1.Node) {
	key := node.UID
	if key == "" {
		key = types.UID(node.Name)
	}
	r.decisionLogsMu.Lock()
	defer r.decisionLogsMu.Unlock()
	delete(r.decisionLogs, key)
}

// effectiveDrainBound returns the drain bound for the candidate, carried on the Command and applied by the queue at
// deletion time: min(matched policy TGP, NodeClaim TGP), or 0 for a forceful policy. nil means the policy sets no
// bound, so the NodeClaim's own TerminationGracePeriod is inherited (the default disruption behavior).
// TODO: the termination-timestamp deadline is a stopgap — replace once the termination flow has a formal contract
// (kubernetes-sigs/karpenter#3029, Formalize Node Termination Contract).
func (r *Repair) effectiveDrainBound(c *Candidate) *time.Duration {
	result, _ := r.matchRepairPolicy(c.Node)
	if result == nil || result.TerminationGracePeriod == nil {
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
