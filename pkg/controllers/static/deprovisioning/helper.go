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

package static

import (
	"cmp"
	"context"
	"math"
	"slices"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

const (
	TerminationReason = "deprovisioned"
)

// Returns nodes suitable for deprovisioning, prioritizing:
// 1. Empty nodes (nodes with no pods or only DaemonSet pods without do-not-disrupt annotation)
// 2. If more nodes needed, nodes with lowest disruption cost (nodes with pods that have do-not-disrupt will have highest cost)
func GetDeprovisioningCandidates(ctx context.Context, kubeClient client.Client, np *v1.NodePool, nodes []*state.StateNode, count int, clk clock.Clock) []*state.StateNode {
	// First get empty nodes
	emptyNodes := lo.Filter(nodes, func(node *state.StateNode, _ int) bool {
		pods, err := node.Pods(ctx, kubeClient)
		if err != nil {
			log.FromContext(ctx).WithValues("node", node.Name()).Error(err, "unable to list pods, treating as non-empty")
			return false
		}
		return len(pods) == 0 || lo.EveryBy(pods, pod.IsOwnedByDaemonSet) && lo.NoneBy(pods, pod.HasDoNotDisrupt)
	})

	candidates := lo.Slice(emptyNodes, 0, count)
	remaining := count - len(candidates)

	if remaining == 0 {
		return candidates
	}

	// Get non-empty nodes with their costs
	type NodeDisruptionCost struct {
		node *state.StateNode
		cost float64
	}

	emptyNodesSet := sets.New(emptyNodes...)
	nonEmptyNodesWithCost := lo.FilterMap(nodes, func(node *state.StateNode, _ int) (NodeDisruptionCost, bool) {
		if _, ok := emptyNodesSet[node]; ok {
			return NodeDisruptionCost{}, false
		}

		pods, err := node.Pods(ctx, kubeClient)
		if err != nil {
			log.FromContext(ctx).WithValues("node", node.Name()).Error(err, "unable to list pods, skipping node")
			return NodeDisruptionCost{}, false
		}

		// Nodes that run pods with do-not-disrupt annotation will receive max disruption cost
		cost := lo.Ternary(lo.SomeBy(pods, pod.HasDoNotDisrupt), math.MaxFloat64,
			disruptionutils.ReschedulingCost(ctx, pods)*
				disruptionutils.LifetimeRemaining(clk, np, node.NodeClaim))

		return NodeDisruptionCost{
			node: node,
			cost: cost,
		}, true
	})

	slices.SortFunc(nonEmptyNodesWithCost, func(i, j NodeDisruptionCost) int {
		return cmp.Compare(i.cost, j.cost)
	})

	// Take the remaining needed nodes with lowest cost
	lowestCostNodes := make([]*state.StateNode, 0, remaining)
	for _, nwc := range nonEmptyNodesWithCost[:remaining] {
		lowestCostNodes = append(lowestCostNodes, nwc.node)
	}

	return append(candidates, lowestCostNodes...)
}
