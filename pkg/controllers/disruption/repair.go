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

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	// repairUnhealthyThreshold stops repair for a NodePool when a correlated failure makes more than this fraction of
	// its nodes unhealthy. Disruption budgets continue to pace concurrent repairs below this safety threshold.
	repairUnhealthyThreshold = "20%"
)

// Repair is a voluntary disruption method that remediates unhealthy nodes. It replaces the standalone node.health
// controller: repair rides the shared disruption budget (reason "Unhealthy"), verifies rescheduling capacity before
// terminating workload-bearing nodes, orders candidates by rank + age/τ, and is vetoed by do-not-repair.
type Repair struct {
	consolidation
	policyMatcher      *health.RepairPolicyMatcher
	decisionLogMonitor *pretty.ChangeMonitor
}

// NewRepair validates and compiles the provider's complete repair policy set before constructing the method. It panics
// when the provider defines no policies or the complete set is invalid.
func NewRepair(c consolidation) *Repair {
	policies := c.cloudProvider.RepairPolicies()
	if len(policies) == 0 {
		panic("node repair requires the cloud provider to define RepairPolicies, but it defines none")
	}
	policyMatcher, err := health.NewRepairPolicyMatcher(policies, sets.New(cloudprovider.ReplaceNode))
	if err != nil {
		panic(fmt.Sprintf("node repair requires valid RepairPolicies: %v", err))
	}
	return &Repair{
		consolidation:      c,
		policyMatcher:      policyMatcher,
		decisionLogMonitor: pretty.NewChangeMonitor(),
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
	now := r.clock.Now()
	evaluation := r.policyMatcher.Evaluate(c.Node, now)
	r.logRepairPolicyDecisions(ctx, c.Node, evaluation.Evaluations)
	if evaluation.Decision == nil {
		return false
	}
	if c.hasPodBlockers && evaluation.Decision.TerminationGracePeriod == nil && c.NodeClaim.Spec.TerminationGracePeriod == nil {
		r.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim,
			"repair requires a termination grace period to bypass blocking pods")...)
		return false
	}
	return true
}

// ComputeCommands orders eligible candidates by the repair score and returns one command for the highest-scoring
// candidate whose NodePool has budget. Workload-bearing candidates verify rescheduling capacity and pre-spin any
// required replacement; empty candidates may produce a delete-only command. Only one command per pass, mirroring drift.
func (r *Repair) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	now := r.clock.Now()
	r.sortCandidates(candidates, now)
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
		command, ok, err := r.commandForCandidate(ctx, candidate, disruptionBudgetMapping)
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
) (Command, bool, error) {
	if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
		return Command{}, false, nil
	}
	candidate, results, ok, err := r.replacementForCandidate(ctx, candidate)
	if err != nil {
		return Command{}, false, err
	}
	if !ok {
		return Command{}, false, nil
	}
	evaluation := r.policyMatcher.Evaluate(candidate.Node, r.clock.Now())
	if evaluation.Decision == nil {
		return Command{}, false, nil
	}
	// Set the candidate's drain bound; after any required replacements are ready, the queue stamps the absolute deadline
	// immediately before requesting deletion. A forceful (0) policy skips the drain for conditions the kubelet can't
	// evict through, without replacement-launch latency eroding the window.
	candidate.TerminationGracePeriod = effectiveDrainBound(candidate, evaluation.Decision)
	candidate.RepairCondition = evaluation.Decision.ConditionType
	return Command{
		Candidates:          []*Candidate{candidate},
		Replacements:        replacementsFromNodeClaims(results.NewNodeClaims...),
		Results:             results,
		PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
	}, true, nil
}

func (r *Repair) sortCandidates(candidates []*Candidate, now time.Time) {
	scores := make(map[*Candidate]float64, len(candidates))
	for _, candidate := range candidates {
		scores[candidate] = r.policyMatcher.Evaluate(candidate.Node, now).Score
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

// breakerTrippedPools returns the NodePools whose unhealthy-node fraction exceeds repairUnhealthyThreshold. A node
// counts as unhealthy as soon as one of its current conditions matches the provider policy set, regardless of policy
// toleration. The threshold rounds up so one unhealthy node does not halt repair in small pools.
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

func (r *Repair) replacementForCandidate(ctx context.Context, candidate *Candidate) (*Candidate, pscheduling.Results, bool, error) {
	if candidate.OwnedByStaticNodePool() {
		current, err := r.revalidateCandidate(ctx, candidate)
		if err != nil || current == nil {
			return nil, pscheduling.Results{}, false, err
		}
		results, ok := r.staticReplacement(current)
		return current, results, ok, nil
	}
	return r.dynamicReplacement(ctx, candidate)
}

func (r *Repair) dynamicReplacement(ctx context.Context, candidate *Candidate) (*Candidate, pscheduling.Results, bool, error) {
	// Repair pre-spins for all reschedulable workload, including pods whose eviction is currently blocked.
	results, err := SimulateScheduling(ctx, r.kubeClient, r.cluster, r.provisioner, r.clock, r.recorder, nil,
		SimulationOptions{IncludeBlockedCandidatePods: true},
		candidate,
	)
	if err != nil {
		if errors.Is(err, errCandidateDeleting) {
			return nil, pscheduling.Results{}, false, nil
		}
		return nil, pscheduling.Results{}, false, err
	}
	if !results.AllNonPendingPodsScheduled() {
		r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
		return nil, pscheduling.Results{}, false, nil
	}
	current, err := r.revalidateCandidate(ctx, candidate)
	if err != nil || current == nil {
		return nil, pscheduling.Results{}, false, err
	}
	// Revalidation refreshes health and PDB admission. If any input that affected the scheduling result also changed,
	// discard this pass instead of pairing fresh candidate state with stale replacement capacity.
	if !sameSchedulingInputs(candidate, current) {
		return nil, pscheduling.Results{}, false, nil
	}
	return current, results, true, nil
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
		if isCandidateValidationError(err) {
			log.FromContext(ctx).V(1).Info("discarding repair candidate after revalidation", "Node", klog.KObj(candidate.Node), "error", err)
			return nil, nil //nolint:nilerr // Candidate validation failures make this candidate stale for the current pass.
		}
		return nil, fmt.Errorf("revalidating repair candidate, %w", err)
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

func (r *Repair) logRepairPolicyDecisions(ctx context.Context, node *corev1.Node, evaluations []health.RepairPolicyEvaluation) {
	logger := log.FromContext(ctx).V(1)
	if !logger.Enabled() {
		return
	}
	decisions := make([][]any, 0, len(evaluations))
	for i := range evaluations {
		decisions = append(decisions, repairPolicyLogValues(&evaluations[i]))
	}
	key := string(node.UID)
	if key == "" {
		key = node.Name
	}
	if len(decisions) == 0 {
		r.decisionLogMonitor.HasChanged(key, decisions)
		return
	}
	if !r.decisionLogMonitor.HasChanged(key, decisions) {
		return
	}
	for _, values := range decisions {
		logger.WithValues(append([]any{
			"Node", klog.KObj(node),
		}, values...)...).Info("evaluated repair policy")
	}
}

func repairPolicyLogValues(result *health.RepairPolicyEvaluation) []any {
	values := []any{
		"condition", result.ConditionType,
		"status", result.ConditionStatus,
		"reason", result.Reason,
		"fallback", result.Fallback,
		"matching-policies", result.MatchingPolicies,
		"eligible-policies", result.EligiblePolicies,
		"action", result.Action,
		"eligible", result.EligiblePolicies != 0,
		"eligible-at", result.EligibleAt,
	}
	if result.TerminationGracePeriod != nil {
		values = append(values, "termination-grace-period", *result.TerminationGracePeriod)
	}
	return values
}

// effectiveDrainBound returns the drain bound for the candidate, carried on the Command and applied by the queue
// immediately before requesting deletion: min(matched policy TGP, NodeClaim TGP), or 0 for a forceful policy. nil
// means the policy sets no bound, so the NodeClaim's own TerminationGracePeriod is inherited (the default disruption
// behavior).
// TODO: the termination-timestamp deadline is a stopgap — replace once the termination flow has a formal contract
// (kubernetes-sigs/karpenter#3029, Formalize Node Termination Contract).
func effectiveDrainBound(c *Candidate, result *health.RepairPolicyEvaluation) *time.Duration {
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
