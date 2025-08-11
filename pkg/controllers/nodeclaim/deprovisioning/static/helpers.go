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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
	pod "sigs.k8s.io/karpenter/pkg/utils/pod"
)

const (
	TerminationReason = "ScaleDown"
)

func GetDeprovisioningCandidates(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode, count int) []*state.StateNode {
	var emptyNodes, nonEmptyNodes []*state.StateNode

	for _, node := range nodes {
		pods, err := node.Pods(ctx, kubeClient)
		if err != nil {
			log.FromContext(ctx).WithValues("node", node.Name()).Error(err, "unable to list pods, treating as non-empty")
			nonEmptyNodes = append(nonEmptyNodes, node)
			continue
		}

		if lo.EveryBy(pods, pod.IsReschedulable) {
			emptyNodes = append(emptyNodes, node)
		} else {
			nonEmptyNodes = append(nonEmptyNodes, node)
		}
	}

	candidates := lo.Slice(emptyNodes, 0, count)

	// If more candidates are needed, use non-empty nodes (prefer oldest)
	if len(candidates) < count {
		remaining := count - len(candidates)
		sort.Slice(nonEmptyNodes, func(i, j int) bool {
			return nonEmptyNodes[i].NodeClaim.CreationTimestamp.Before(&nonEmptyNodes[j].NodeClaim.CreationTimestamp)
		})
		candidates = append(candidates, lo.Slice(nonEmptyNodes, 0, remaining)...)
	}

	return candidates
}
