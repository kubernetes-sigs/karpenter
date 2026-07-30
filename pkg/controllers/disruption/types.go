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
	"math"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/option"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/google/uuid"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

const (
	GracefulDisruptionClass = "graceful" // graceful disruption always respects blocking pod PDBs and the do-not-disrupt annotation
	EventualDisruptionClass = "eventual" // eventual disruption is bounded by a NodePool's TerminationGracePeriod, regardless of blocking pod PDBs and the do-not-disrupt annotation
)

type MethodOptions struct {
	validator Validator
}

func WithValidator(v Validator) option.Function[MethodOptions] {
	return func(o *MethodOptions) {
		o.validator = v
	}
}

type Method interface {
	ShouldDisrupt(context.Context, *Candidate) bool
	ComputeCommands(context.Context, map[string]int, ...*Candidate) ([]Command, error)
	Reason() v1.DisruptionReason
	Class() string
	ConsolidationType() string
}

type CandidateFilter func(context.Context, *Candidate) bool

// Candidate is a state.StateNode that we are considering for disruption along with extra information to be used in
// making that determination
type Candidate struct {
	*state.StateNode
	instanceType      *cloudprovider.InstanceType
	NodePool          *v1.NodePool
	zone              string
	capacityType      string
	DisruptionCost    float64
	reschedulablePods []*corev1.Pod

	// Price is the cheapest compatible offering price for this candidate.
	// Precomputed at creation to avoid repeated offering lookups.
	Price float64
	// RescheduleDisruptionCost is 1.0 (base) + sum of positive pod eviction costs
	// for reschedulable pods. Used by balanced scoring.
	RescheduleDisruptionCost float64
}

// ScoreResult holds the three values needed to decide whether a move passes.
type ScoreResult struct {
	SavingsFraction    float64
	DisruptionFraction float64
	K                  int32
}

// Score is savings/disruption, guarded for zero denominators and non-positive savings.
func (r ScoreResult) Score() float64 {
	if r.SavingsFraction <= 0 {
		return 0
	}
	if r.DisruptionFraction == 0 {
		return math.Inf(1)
	}
	return r.SavingsFraction / r.DisruptionFraction
}

func (r ScoreResult) Threshold() float64 { return 1.0 / float64(r.K) }
func (r ScoreResult) Approved() bool     { return r.Score() >= r.Threshold() }

// resolveNodePrice returns the actual price of a running node by looking up the
// offering that matches the node's zone and capacity-type labels.
// Returns 0 when the instance type is nil or no matching offering exists.
func resolveNodePrice(node *state.StateNode, instanceType *cloudprovider.InstanceType) float64 {
	if instanceType == nil {
		return 0
	}
	labels := node.Labels()
	price, ok := instanceType.OfferingPrice(labels[corev1.LabelTopologyZone], labels[v1.CapacityTypeLabelKey])
	if !ok {
		return 0
	}
	if math.IsNaN(price) {
		return 0
	}
	return price
}

// PerNodeBaseDisruptionCost is the inherent cost of draining a node (cordon,
// drain, API calls, replacement latency). Could become per-NodePool if GPU
// nodes need higher weight. See designs/balanced-consolidation.md.
const PerNodeBaseDisruptionCost = 1.0

func computeRescheduleDisruptionCost(ctx context.Context, reschedulablePods []*corev1.Pod) float64 {
	cost := PerNodeBaseDisruptionCost
	for _, p := range reschedulablePods {
		cost += math.Max(0, disruptionutils.EvictionCost(ctx, p))
	}
	return cost
}

// SavingsRatio returns cost per unit disruption (higher = prefer to disrupt).
func (c *Candidate) SavingsRatio() float64 { return c.Price / c.RescheduleDisruptionCost }

func (c *Candidate) OwnedByStaticNodePool() bool {
	return c.NodePool.Spec.Replicas != nil
}

// IsEmpty reports that no pod contributes positive reschedule disruption cost.
// A node with running pods whose eviction costs all clamp to zero is Empty under
// this definition; the Empty disruption reason and Empty budgets govern its
// deletion through the Emptiness method.
func (c *Candidate) IsEmpty() bool {
	return c.RescheduleDisruptionCost <= PerNodeBaseDisruptionCost
}

//nolint:gocyclo
func NewCandidate(ctx context.Context, kubeClient client.Client, recorder events.Recorder, clk clock.Clock, node *state.StateNode, pdbs pdb.Limits,
	nodePoolMap map[string]*v1.NodePool, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType, queue *Queue, disruptionClass string,
) (*Candidate, error) {
	var err error
	var pods []*corev1.Pod
	// If the orchestration queue is already considering a candidate we want to disrupt, don't consider it a candidate.
	if queue.HasAny(node.ProviderID()) {
		return nil, fmt.Errorf("candidate is already being disrupted")
	}
	if err = node.ValidateNodeDisruptable(clk); err != nil {
		// Only emit an event if the NodeClaim is not nil, ensuring that we only emit events for Karpenter-managed nodes
		if node.NodeClaim != nil {
			recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, pretty.Sentence(err.Error()))...)
		}
		return nil, err
	}
	// We know that the node will have the label key because of the node.IsDisruptable check above
	nodePoolName := node.Labels()[v1.NodePoolLabelKey]
	// nodePool is a shared cache-backed pointer; treat as read-only. DeepCopy before mutating.
	nodePool := nodePoolMap[nodePoolName]
	instanceTypeMap := nodePoolToInstanceTypesMap[nodePoolName]
	// skip any candidates where we can't determine the nodePool
	if nodePool == nil || instanceTypeMap == nil {
		recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, fmt.Sprintf("NodePool not found (NodePool=%s)", nodePoolName))...)
		return nil, serrors.Wrap(fmt.Errorf("nodepool not found"), "NodePool", klog.KRef("", nodePoolName))
	}
	// We only care if instanceType in non-empty consolidation to do price-comparison.
	instanceType := instanceTypeMap[node.Labels()[corev1.LabelInstanceTypeStable]]
	if pods, err = node.ValidatePodsDisruptable(ctx, kubeClient, pdbs, clk, recorder); err != nil {
		// If the NodeClaim has a TerminationGracePeriod set and the disruption class is eventual, the node should be
		// considered a candidate even if there's a pod that will block eviction. Other error types should still cause
		// failure creating the candidate.
		eventualDisruptionCandidate := node.NodeClaim.Spec.TerminationGracePeriod != nil && disruptionClass == EventualDisruptionClass
		if lo.Ternary(eventualDisruptionCandidate, state.IgnorePodBlockEvictionError(err), err) != nil {
			recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, pretty.Sentence(err.Error()))...)
			return nil, err
		}
	}
	reschedulable := lo.Filter(pods, func(p *corev1.Pod, _ int) bool { return pod.IsReschedulable(p) })
	return &Candidate{
		StateNode:         node,
		instanceType:      instanceType,
		NodePool:          nodePool,
		capacityType:      node.Labels()[v1.CapacityTypeLabelKey],
		zone:              node.Labels()[corev1.LabelTopologyZone],
		reschedulablePods: reschedulable,
		// We get the disruption cost from all pods in the candidate, not just the reschedulable pods
		DisruptionCost:           disruptionutils.ReschedulingCost(ctx, pods) * disruptionutils.LifetimeRemaining(clk, nodePool, node.NodeClaim),
		Price:                    resolveNodePrice(node, instanceType),
		RescheduleDisruptionCost: computeRescheduleDisruptionCost(ctx, reschedulable),
	}, nil
}

type Replacement struct {
	*pscheduling.NodeClaim

	Name string
	// Use a bool track if a node has already been initialized so we can fire metrics for initialization once.
	// This intentionally does not capture nodes that go initialized then go NotReady after as other pods can
	// schedule to this node as well.
	Initialized bool
}

func replacementsFromNodeClaims(newNodeClaims ...*pscheduling.NodeClaim) []*Replacement {
	return lo.Map(newNodeClaims, func(n *pscheduling.NodeClaim, _ int) *Replacement { return &Replacement{NodeClaim: n} })
}

type Command struct {
	Method

	Succeeded bool

	CreationTimestamp time.Time
	ID                uuid.UUID

	Results             pscheduling.Results
	Candidates          []*Candidate
	Replacements        []*Replacement
	PoolDisruptionCosts map[string]float64
}

// Reason returns the disruption reason for this command.
func (c Command) Reason() v1.DisruptionReason {
	if c.Method == nil {
		return ""
	}
	return c.Method.Reason()
}

type Decision string

var (
	NoOpDecision    Decision = "no-op"
	ReplaceDecision Decision = "replace"
	DeleteDecision  Decision = "delete"
)

func (c Command) Decision() Decision {
	switch {
	case len(c.Candidates) > 0 && len(c.Replacements) > 0:
		return ReplaceDecision
	case len(c.Candidates) > 0 && len(c.Replacements) == 0:
		return DeleteDecision
	default:
		return NoOpDecision
	}
}

// PoolDisruptionCost returns the disruption cost for a given pool. If the
// pre-computed map is populated, it reads from there; otherwise it falls back
// to computing from candidates (for backwards compat in tests).
func (c Command) PoolDisruptionCost(poolName string) float64 {
	if c.PoolDisruptionCosts != nil {
		return c.PoolDisruptionCosts[poolName]
	}
	// Fallback: compute from candidates directly.
	var cost float64
	for _, cand := range c.Candidates {
		if cand.NodePool.Name == poolName {
			cost += cand.RescheduleDisruptionCost
		}
	}
	return cost
}

// computePoolDisruptionCosts groups candidates by NodePool name and sums
// RescheduleDisruptionCost per group.
func computePoolDisruptionCosts(candidates []*Candidate) map[string]float64 {
	if len(candidates) == 0 {
		return nil
	}
	costs := make(map[string]float64, len(candidates))
	for _, c := range candidates {
		costs[c.NodePool.Name] += c.RescheduleDisruptionCost
	}
	return costs
}

// SourceNodeNames returns the names of all candidate nodes
func (c Command) SourceNodeNames() []string {
	return lo.Map(c.Candidates, func(candidate *Candidate, _ int) string {
		return candidate.Name()
	})
}

// String returns a human-readable representation of the command
func (c Command) String() string {
	sources := strings.Join(c.SourceNodeNames(), ", ")
	nodePools := strings.Join(lo.Uniq(lo.FilterMap(c.Candidates, func(candidate *Candidate, _ int) (string, bool) {
		if candidate.NodePool == nil {
			return "", false
		}
		return candidate.NodePool.Name, true
	})), ",")

	// For test commands without Method/ID set, use simple format
	if c.Method == nil {
		if len(c.Replacements) > 0 {
			plural := "replacements"
			if len(c.Replacements) == 1 {
				plural = "replacement"
			}
			return fmt.Sprintf("%s: [%s] -> [%d %s]", c.Decision(), sources, len(c.Replacements), plural)
		}
		return fmt.Sprintf("%s: [%s]", c.Decision(), sources)
	}

	// Full format with reason, ID, nodepools, and savings
	if len(c.Replacements) > 0 {
		plural := "replacements"
		if len(c.Replacements) == 1 {
			plural = "replacement"
		}
		return fmt.Sprintf("%s/%s: %s: nodepools=[%s]: [%s] -> [%d %s] (savings: $%.2f)", c.Reason(), c.ID, c.Decision(), nodePools, sources, len(c.Replacements), plural, c.EstimatedSavings())
	}
	return fmt.Sprintf("%s/%s: %s: nodepools=[%s]: [%s] (savings: $%.2f)", c.Reason(), c.ID, c.Decision(), nodePools, sources, c.EstimatedSavings())
}

// StringForNode returns a string representation of the command from the perspective of a single source candidate node.
// For single-node commands, returns the full command string. For multi-node commands, returns a per-node
// string with context to avoid listing all nodes.
//
// Note: This method only works for source nodes (Candidates being removed). Consolidation destinations can be
// either new nodes (Replacements) or existing nodes (ExistingNodes in scheduling.Results), but only Replacements
// are tracked in the Command. Events are currently only emitted for source nodes, not destinations.
func (c Command) StringForNode(candidate *Candidate) string {
	if len(c.Candidates) == 1 {
		return c.String()
	}
	// Multi-node: show only this node with context
	return fmt.Sprintf("%s: [%s] (part of %d-node consolidation)", c.Decision(), candidate.Name(), len(c.Candidates))
}

// SourceCost sums Price across all candidates in this command.
func (c Command) SourceCost() float64 {
	return lo.SumBy(c.Candidates, func(cd *Candidate) float64 { return cd.Price })
}

// EstimatedSavings returns the estimated cost savings from this consolidation.
// Returns 0.0 when pricing cannot be determined.
func (c Command) EstimatedSavings() float64 {
	sourcePrice := c.SourceCost()

	// For delete consolidation, all source cost is savings
	if len(c.Replacements) == 0 {
		return sourcePrice
	}

	// For replace consolidation, sum destination costs from all replacement NodeClaims.
	// Filter to available offerings so ICE'd zones don't produce an optimistic estimate.
	destPrice := 0.0
	for _, nodeClaim := range c.Results.NewNodeClaims {
		if len(nodeClaim.InstanceTypeOptions) > 0 {
			available := nodeClaim.InstanceTypeOptions[0].Offerings.Available()
			if len(available) > 0 {
				destPrice += available.Cheapest().Price
			}
		}
	}

	return sourcePrice - destPrice
}

// EmitCandidateEvents emits ConsolidationCandidate events for all candidates in this command
func (c Command) EmitCandidateEvents(recorder events.Recorder) {
	for _, candidate := range c.Candidates {
		recorder.Publish(disruptionevents.ConsolidationCandidate(candidate.Node, candidate.NodeClaim, c.StringForNode(candidate), c.EstimatedSavings())...)
	}
}

// EmitRejectedEvents emits ConsolidationRejected events for all candidates in this command
func (c Command) EmitRejectedEvents(recorder events.Recorder, reason string) {
	for _, candidate := range c.Candidates {
		recorder.Publish(disruptionevents.ConsolidationRejected(candidate.Node, candidate.NodeClaim, c.StringForNode(candidate), reason, c.EstimatedSavings())...)
	}
}

func (c Command) LogValues() []any {
	podCount := lo.Reduce(c.Candidates, func(acc int, cd *Candidate, _ int) int { return acc + len(cd.reschedulablePods) }, 0)

	candidateNodes := lo.Map(c.Candidates, func(candidate *Candidate, _ int) any {
		return map[string]any{
			"Node":          klog.KObj(candidate.Node),
			"NodeClaim":     klog.KObj(candidate.NodeClaim),
			"instance-type": candidate.Labels()[corev1.LabelInstanceTypeStable],
			"capacity-type": candidate.Labels()[v1.CapacityTypeLabelKey],
		}
	})
	replacementNodes := lo.Map(c.Replacements, func(replacement *Replacement, _ int) any {
		ct := replacement.Requirements.Get(v1.CapacityTypeLabelKey)
		m := map[string]any{
			"capacity-type": lo.If(
				ct.Has(v1.CapacityTypeReserved), v1.CapacityTypeReserved,
			).ElseIf(
				ct.Has(v1.CapacityTypeSpot), v1.CapacityTypeSpot,
			).Else(v1.CapacityTypeOnDemand),
		}
		if len(c.Replacements) == 1 {
			m["instance-types"] = pscheduling.InstanceTypeList(replacement.InstanceTypeOptions)
		}
		return m
	})

	return []any{
		"decision", c.Decision(),
		"disrupted-node-count", len(candidateNodes),
		"replacement-node-count", len(replacementNodes),
		"pod-count", podCount,
		"disrupted-nodes", candidateNodes,
		"replacement-nodes", replacementNodes,
	}
}
