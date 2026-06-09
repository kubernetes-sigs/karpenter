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
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

const (
	// DisruptionCostAnnotation is the user-facing Karpenter annotation for expressing
	// the cost of evicting a pod during consolidation. Customers set this on workloads
	// to influence which pods Karpenter prefers to evict during consolidation.
	DisruptionCostAnnotation = apis.Group + "/disruption-cost"
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
//
// The PodDeletionCostManagement feature gate determines which annotations are read:
//
//   - Gate ON: Karpenter's pod-deletion-cost controller writes
//     controller.kubernetes.io/pod-deletion-cost on managed pods to influence the
//     ReplicaSet controller's scale-down ordering. Those values reflect RS coordination
//     ranking, not user intent about consolidation cost. Consolidation scoring therefore
//     reads only karpenter.sh/disruption-cost.
//
//   - Gate OFF (default): the controller does not write pod-deletion-cost, so any
//     existing values are user-set. Consolidation scoring reads
//     karpenter.sh/disruption-cost first; if absent, it falls back to
//     controller.kubernetes.io/pod-deletion-cost. This preserves current behavior for
//     customers who have not migrated to the new annotation.
func EvictionCost(ctx context.Context, p *corev1.Pod) float64 {
	cost := 1.0
	if costStr, ok := p.Annotations[DisruptionCostAnnotation]; ok {
		parsedCost, err := strconv.ParseFloat(costStr, 64)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed parsing disruption cost",
				"annotation", DisruptionCostAnnotation, "value", costStr, "pod", client.ObjectKeyFromObject(p))
		} else {
			cost += parsedCost / math.Pow(2, 27.0)
		}
	} else if !options.FromContext(ctx).FeatureGates.PodDeletionCostManagement {
		if podDeletionCostStr, ok := p.Annotations[corev1.PodDeletionCost]; ok {
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
