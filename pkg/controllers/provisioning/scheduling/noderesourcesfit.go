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

package scheduling

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// maxNodeScore mirrors kube-scheduler's MaxNodeScore.
const maxNodeScore int64 = 100

// NodeResourcesFitScorer mirrors the LeastAllocated and MostAllocated scoring strategies of kube-scheduler's
// NodeResourcesFit plugin. The scheduler tries existing nodes in descending score order, so the first node a pod fits on
// is the one kube-scheduler would rank highest on resource fit.
//
// Unlike kube-scheduler, the score is computed without the pod being placed, so one ordering serves every pod. For the
// same reason only cpu, memory and ephemeral-storage are scored: kube-scheduler scores the other (scalar) resources only
// for pods that request them.
type NodeResourcesFitScorer struct {
	strategy  karpopts.ScoringStrategyType
	resources []karpopts.ResourceSpec
}

// NewNodeResourcesFitScorer returns nil when no scoring strategy is configured, in which case existing nodes keep their
// name order.
func NewNodeResourcesFitScorer(ctx context.Context) *NodeResourcesFitScorer {
	cfg := karpopts.FromContext(ctx).SchedulerConfig
	if cfg == nil || cfg.NodeResourcesFit == nil || cfg.NodeResourcesFit.ScoringStrategy == nil {
		return nil
	}
	return &NodeResourcesFitScorer{
		strategy:  cfg.NodeResourcesFit.ScoringStrategy.Type,
		resources: cfg.NodeResourcesFit.ScoringStrategy.Resources,
	}
}

// Score mirrors kube-scheduler's leastResourceScorer and mostResourceScorer, including their integer arithmetic.
func (s *NodeResourcesFitScorer) Score(n *ExistingNode) int64 {
	allocatable := n.Allocatable()
	var nodeScore, weightSum int64
	for _, r := range s.resources {
		name := corev1.ResourceName(r.Name)
		if name != corev1.ResourceCPU && name != corev1.ResourceMemory && name != corev1.ResourceEphemeralStorage {
			continue
		}
		capacity := resources.ScaledValue(name, allocatable[name])
		if capacity == 0 {
			continue
		}
		// The node's remaining resources already account for the pods bound to it, the pods simulated onto it, and
		// the daemonset pods expected to land on it.
		requested := max(capacity-resources.ScaledValue(name, n.remainingResources[name]), 0)
		nodeScore += s.resourceScore(requested, capacity) * r.Weight
		weightSum += r.Weight
	}
	if weightSum == 0 {
		return 0
	}
	return nodeScore / weightSum
}

func (s *NodeResourcesFitScorer) resourceScore(requested, capacity int64) int64 {
	if s.strategy == karpopts.MostAllocated {
		return min(requested, capacity) * maxNodeScore / capacity
	}
	if requested > capacity {
		return 0
	}
	return (capacity - requested) * maxNodeScore / capacity
}
