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
	c.RepairPolicyResult = r.evaluate(ctx, c.Node, now)
	if c.RepairPolicyResult.Action == "" {
		return false
	}
	if c.hasPodBlockers && c.RepairPolicyResult.TerminationGracePeriod == nil && c.NodeClaim.Spec.TerminationGracePeriod == nil {
		r.recorder.Publish(disruptionevents.Blocked(c.Node, c.NodeClaim,
			"repair requires a termination grace period to bypass blocking pods")...)
		return false
	}
	return true
}

func (r *Repair) evaluate(ctx context.Context, node *corev1.Node, now time.Time) health.RepairResult {
	result := r.policyMatcher.Evaluate(node, now)
	logger := log.FromContext(ctx).V(1)
	if !logger.Enabled() {
		return result
	}
	key := string(node.UID)
	if key == "" {
		key = node.Name
	}
	var values []any
	if result.Action != "" {
		values = []any{
			"condition", result.Condition,
			"action", result.Action,
			"earliest-eligible-at", result.EligibleAt,
		}
		if result.TerminationGracePeriod != nil {
			values = append(values, "termination-grace-period", *result.TerminationGracePeriod)
		}
	}
	if !r.decisionLogMonitor.HasChanged(key, values) || len(values) == 0 {
		return result
	}
	logger.WithValues(append([]any{
		"Node", klog.KObj(node),
	}, values...)...).Info("evaluated repair policy")
	return result
}

// ComputeCommands orders eligible candidates by the repair score and returns one command for the highest-scoring
// candidate whose NodePool has budget. Workload-bearing candidates verify rescheduling capacity and pre-spin any
// required replacement; empty candidates may produce a delete-only command. Only one command per pass, mirroring drift.
//
//nolint:gocyclo // Static and dynamic replacement flows are intentionally kept inline.
func (r *Repair) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
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

		var results pscheduling.Results
		if candidate.OwnedByStaticNodePool() {
			nodeLimit := int64(math.MaxInt64)
			if limit, ok := candidate.NodePool.Spec.Limits[resources.Node]; ok {
				nodeLimit = limit.Value()
			}
			runningNodes, _, pendingDisruption := r.cluster.NodePoolState.GetNodeCount(candidate.NodePool.Name)
			if int64(runningNodes+pendingDisruption) > lo.FromPtr(candidate.NodePool.Spec.Replicas) {
				r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim,
					fmt.Sprintf("static NodePool %q is still scaling down", candidate.NodePool.Name))...)
				continue
			}
			if r.cluster.NodePoolState.ReserveNodeCount(candidate.NodePool.Name, nodeLimit, 1) == 0 {
				r.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim,
					fmt.Sprintf("static NodePool %q has no node limit available for a replacement", candidate.NodePool.Name))...)
				continue
			}
			template := pscheduling.NewNodeClaimTemplate(candidate.NodePool)
			results = pscheduling.Results{
				NewNodeClaims: []*pscheduling.NodeClaim{{NodeClaimTemplate: *template}},
			}
		} else {
			// Repair pre-spins for all reschedulable workload, including pods whose eviction is currently blocked.
			results, err = SimulateScheduling(ctx, r.kubeClient, r.cluster, r.provisioner, r.clock, r.recorder, nil,
				SimulationOptions{IncludeBlockedCandidatePods: true},
				candidate,
			)
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
		}

		// Set the candidate's drain bound; after any required replacements are ready, the queue stamps the absolute
		// deadline immediately before requesting deletion. A forceful (0) policy skips the drain for conditions the
		// kubelet can't evict through, without replacement-launch latency eroding the window.
		candidate.TerminationGracePeriod = effectiveDrainBound(candidate, candidate.RepairPolicyResult)
		return []Command{{
			Candidates:          []*Candidate{candidate},
			Replacements:        replacementsFromNodeClaims(results.NewNodeClaims...),
			Results:             results,
			PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
		}}, nil
	}
	return []Command{}, nil
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
