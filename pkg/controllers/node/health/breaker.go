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

package health

import (
	"context"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// UnhealthyThreshold stops repair for a NodePool when a correlated failure makes more than this fraction of its nodes
// unhealthy. It is shared by every repair path so they agree on when a NodePool is too unhealthy to repair.
const UnhealthyThreshold = "20%"

// TrippedNodePools returns the NodePools whose unhealthy-node fraction exceeds UnhealthyThreshold, considering the
// nodes selected by opts. A node counts as unhealthy as soon as one of its current conditions matches the provider
// policy set, regardless of policy toleration or initialization. The threshold rounds up so one unhealthy node does not
// halt repair in small pools.
func TrippedNodePools(ctx context.Context, kubeClient client.Client, matcher *RepairPolicyMatcher, opts ...client.ListOption) (map[string]bool, error) {
	// TODO: cache unhealthy node counts by NodePool from Node updates instead of recalculating them on every repair pass.
	nodeList := &corev1.NodeList{}
	if err := kubeClient.List(ctx, nodeList, append(opts, client.UnsafeDisableDeepCopy)...); err != nil {
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
		if lo.SomeBy(node.Status.Conditions, matcher.Matches) {
			unhealthy[nodePool]++
		}
	}
	tripped := map[string]bool{}
	thresholdValue := intstr.FromString(UnhealthyThreshold)
	for nodePool, count := range total {
		threshold := lo.Must(intstr.GetScaledValueFromIntOrPercent(&thresholdValue, count, true))
		if unhealthy[nodePool] > threshold {
			tripped[nodePool] = true
		}
	}
	return tripped, nil
}
