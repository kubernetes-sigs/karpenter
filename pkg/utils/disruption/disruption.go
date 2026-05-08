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
	"math"
	"strconv"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	// ConsolidationPriorityAnnotation is the user-facing annotation for expressing
	// consolidation disruption cost, separate from the auto-managed pod-deletion-cost.
	ConsolidationPriorityAnnotation = apis.Group + "/consolidation-priority"
	// ManagedDeletionCostAnnotation indicates the pod-deletion-cost is managed by Karpenter's
	// deletion cost controller (for RS coordination), not user-set for consolidation steering.
	ManagedDeletionCostAnnotation = apis.Group + "/managed-deletion-cost"
)

// lifetimeRemaining calculates the fraction of node lifetime remaining in the range [0.0, 1.0].  If the ExpireAfter
// is non-zero, we use it to scale down the disruption costs of candidates that are going to expire.  Just after creation, the
// disruption cost is highest, and it approaches zero as the node ages towards its expiration time.
func LifetimeRemaining(clock clock.Clock, nodePool *v1.NodePool, nodeClaim *v1.NodeClaim) float64 {
	remaining := 1.0
	if nodeClaim.Spec.ExpireAfter.Duration != nil {
		ageInSeconds := clock.Since(nodeClaim.CreationTimestamp.Time).Seconds()
		totalLifetimeSeconds := nodeClaim.Spec.ExpireAfter.Seconds()
		lifetimeRemainingSeconds := totalLifetimeSeconds - ageInSeconds
		remaining = lo.Clamp(lifetimeRemainingSeconds/totalLifetimeSeconds, 0.0, 1.0)
	}
	return remaining
}

// EvictionCost returns the disruption cost computed for evicting the given pod.
// When karpenter.sh/consolidation-priority is set, it takes precedence over pod-deletion-cost
// for consolidation scoring. When pod-deletion-cost is auto-managed by Karpenter (indicated by
// the managed-deletion-cost sentinel), it is ignored for consolidation scoring since it reflects
// RS coordination ranking, not user intent about disruption cost.
func EvictionCost(ctx context.Context, p *corev1.Pod) float64 {
	cost := 1.0
	if costStr, ok := p.Annotations[ConsolidationPriorityAnnotation]; ok {
		parsedCost, err := strconv.ParseFloat(costStr, 64)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed parsing consolidation priority",
				"annotation", ConsolidationPriorityAnnotation, "value", costStr, "pod", client.ObjectKeyFromObject(p))
		} else {
			cost += parsedCost / math.Pow(2, 27.0)
		}
	} else if podDeletionCostStr, ok := p.Annotations[corev1.PodDeletionCost]; ok {
		if _, managed := p.Annotations[ManagedDeletionCostAnnotation]; !managed {
			podDeletionCost, err := strconv.ParseFloat(podDeletionCostStr, 64)
			if err != nil {
				log.FromContext(ctx).Error(err, "failed parsing pod deletion cost",
					"annotation", corev1.PodDeletionCost, "value", podDeletionCostStr, "pod", client.ObjectKeyFromObject(p))
			} else {
				cost += podDeletionCost / math.Pow(2, 27.0)
			}
		}
	}
	if p.Spec.Priority != nil {
		cost += float64(*p.Spec.Priority) / math.Pow(2, 25)
	}

	return lo.Clamp(cost, -10.0, 10.0)
}

func ReschedulingCost(ctx context.Context, pods []*corev1.Pod) float64 {
	cost := 0.0
	for _, p := range pods {
		cost += EvictionCost(ctx, p)
	}
	return cost
}
