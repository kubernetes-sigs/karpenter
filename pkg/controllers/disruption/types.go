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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

const (
	GracefulDisruptionClass = "graceful" // graceful disruption always respects blocking pod PDBs and the do-not-disrupt annotation
	EventualDisruptionClass = "eventual" // eventual disruption is bounded by a NodePool's TerminationGracePeriod, regardless of blocking pod PDBs and the do-not-disrupt annotation
)

type Method interface {
	ShouldDisrupt(context.Context, *Candidate) bool
	ComputeCommand(context.Context, map[string]int, ...*Candidate) (Command, scheduling.Results, error)
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
}

//nolint:gocyclo
func NewCandidate(ctx context.Context, clk clock.Clock, node *state.StateNode, nodePoolMap map[string]*v1.NodePool,
	nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType) *Candidate {
	var pods []*corev1.Pod
	// We know that the node will have the label key because of the node.IsDisruptable check above
	nodePoolName := node.Labels()[v1.NodePoolLabelKey]
	nodePool := nodePoolMap[nodePoolName]
	instanceTypeMap := nodePoolToInstanceTypesMap[nodePoolName]
	// We only care if instanceType in non-empty consolidation to do price-comparison.
	instanceType := instanceTypeMap[node.Labels()[corev1.LabelInstanceTypeStable]]
	return &Candidate{
		StateNode:         node.DeepCopy(),
		instanceType:      instanceType,
		NodePool:          nodePool,
		capacityType:      node.Labels()[v1.CapacityTypeLabelKey],
		zone:              node.Labels()[corev1.LabelTopologyZone],
		reschedulablePods: lo.Filter(pods, func(p *corev1.Pod, _ int) bool { return pod.IsReschedulable(p) }),
		// We get the disruption cost from all pods in the candidate, not just the reschedulable pods
		DisruptionCost: disruptionutils.ReschedulingCost(ctx, pods) * disruptionutils.LifetimeRemaining(clk, nodePool, node.NodeClaim),
	}
}

type Command struct {
	candidates   []*Candidate
	replacements []*scheduling.NodeClaim
}

type Decision string

var (
	NoOpDecision    Decision = "no-op"
	ReplaceDecision Decision = "replace"
	DeleteDecision  Decision = "delete"
)

func (c Command) Decision() Decision {
	switch {
	case len(c.candidates) > 0 && len(c.replacements) > 0:
		return ReplaceDecision
	case len(c.candidates) > 0 && len(c.replacements) == 0:
		return DeleteDecision
	default:
		return NoOpDecision
	}
}

func (c Command) LogValues() []any {
	podCount := lo.Reduce(c.candidates, func(_ int, cd *Candidate, _ int) int { return len(cd.reschedulablePods) }, 0)

	candidateNodes := lo.Map(c.candidates, func(candidate *Candidate, _ int) interface{} {
		return map[string]interface{}{
			"Node":          klog.KObj(candidate.Node),
			"NodeClaim":     klog.KObj(candidate.NodeClaim),
			"instance-type": candidate.Labels()[corev1.LabelInstanceTypeStable],
			"capacity-type": candidate.Labels()[v1.CapacityTypeLabelKey],
		}
	})
	replacementNodes := lo.Map(c.replacements, func(replacement *scheduling.NodeClaim, _ int) interface{} {
		ct := replacement.Requirements.Get(v1.CapacityTypeLabelKey)
		m := map[string]interface{}{
			"capacity-type": lo.If(
				ct.Has(v1.CapacityTypeReserved), v1.CapacityTypeReserved,
			).ElseIf(
				ct.Has(v1.CapacityTypeSpot), v1.CapacityTypeSpot,
			).Else(v1.CapacityTypeOnDemand),
		}
		if len(c.replacements) == 1 {
			m["instance-types"] = scheduling.InstanceTypeList(replacement.InstanceTypeOptions)
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
