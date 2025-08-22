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
	"context"
	"sort"

	"github.com/samber/lo"

	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
	pod "sigs.k8s.io/karpenter/pkg/utils/pod"
)

const (
	TerminationReason = "ScaleDown"
)

// Get non-empty nodes with their costs
type NodeDisruptionCost struct {
	node *state.StateNode
	cost float64
}

// Returns nodes suitable for deprovisioning, prioritizing:
// 1. Empty nodes (nodes with no pods or only DaemonSet pods)
// 2. If more nodes needed, nodes with lowest disruption cost
func GetDeprovisioningCandidates(ctx context.Context, kubeClient client.Client, np *v1.NodePool, nodes []*state.StateNode, count int, clk clock.Clock) []*state.StateNode {
	// First get empty nodes
	emptyNodes := lo.Filter(nodes, func(node *state.StateNode, _ int) bool {
		pods, err := node.Pods(ctx, kubeClient)
		if err != nil {
			log.FromContext(ctx).WithValues("node", node.Name()).Error(err, "unable to list pods, treating as non-empty")
			return false
		}
		return len(pods) == 0 || lo.EveryBy(pods, pod.IsOwnedByDaemonSet)
	})
	emptyNodesSet := lo.SliceToMap(emptyNodes, func(n *state.StateNode) (*state.StateNode, struct{}) {
		return n, struct{}{}
	})
	candidates := lo.Slice(emptyNodes, 0, count)
	remaining := count - len(candidates)

	if remaining > 0 {
		nonEmptyNodesWithCost := lo.FilterMap(nodes, func(node *state.StateNode, _ int) (NodeDisruptionCost, bool) {
			if _, ok := emptyNodesSet[node]; ok {
				return NodeDisruptionCost{}, false
			}

			pods, err := node.Pods(ctx, kubeClient)
			if err != nil {
				log.FromContext(ctx).WithValues("node", node.Name()).Error(err, "unable to list pods, skipping node")
				return NodeDisruptionCost{}, false
			}

			return NodeDisruptionCost{
				node: node,
				cost: disruptionutils.ReschedulingCost(ctx, pods) *
					disruptionutils.LifetimeRemaining(clk, np, node.NodeClaim),
			}, true
		})

		// Optimization : Dont need to sort all items, instead use a heap to get remaining
		sort.Slice(nonEmptyNodesWithCost, func(i, j int) bool {
			return nonEmptyNodesWithCost[i].cost < nonEmptyNodesWithCost[j].cost
		})

		// Take the remaining needed nodes with lowest cost
		lowestCostNodes := lo.Map(
			lo.Slice(nonEmptyNodesWithCost, 0, remaining),
			func(nwc NodeDisruptionCost, _ int) *state.StateNode {
				return nwc.node
			},
		)

		candidates = append(candidates, lowestCostNodes...)
	}
	return candidates
}

func RunningNodesForNodePool(c *state.Cluster, np *v1.NodePool) int64 {
	running, _ := c.NodePoolState.GetNodeCount(np.Name)
	return int64(running)
}
