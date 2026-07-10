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

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/options"
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
	if costStr, ok := p.Annotations[v1.DisruptionCostAnnotationKey]; ok {
		parsedCost, err := strconv.ParseFloat(costStr, 64)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed parsing disruption cost",
				"annotation", v1.DisruptionCostAnnotationKey, "value", costStr, "pod", client.ObjectKeyFromObject(p))
		} else {
			// 2^27 = max representable pod-deletion-cost (int32 ceiling).
			// Dividing here normalizes the user value into [0, 1) so it
			// cannot overpower the QoS/priority bands above it.
			cost += parsedCost / math.Pow(2, 27.0)
		}
	} else if !podDeletionCostManagementEnabled(ctx) {
		if podDeletionCostStr, ok := p.Annotations[corev1.PodDeletionCost]; ok {
			podDeletionCost, err := strconv.ParseFloat(podDeletionCostStr, 64)
			if err != nil {
				log.FromContext(ctx).Error(err, "failed parsing pod deletion cost",
					"annotation", corev1.PodDeletionCost, "value", podDeletionCostStr, "pod", client.ObjectKeyFromObject(p))
			} else {
				// Same 2^27 normalization as above; legacy fall-back for
				// gate-OFF behavior reads the upstream annotation.
				cost += podDeletionCost / math.Pow(2, 27.0)
			}
		}
	}
	if p.Spec.Priority != nil {
		// 2^25 places priority in a band that exceeds the user-annotation
		// band (2^27 divisor) but stays under the QoS band, so priority
		// dominates user steering without overwhelming QoS classification.
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

// podDeletionCostManagementEnabled reports whether the PodDeletionCostManagement feature
// gate is enabled on the ctx's options. When options are absent (e.g. tests that construct
// raw contexts without going through the operator injector), the gate is treated as its
// default OFF value so the legacy pod-deletion-cost fallback path remains callable without
// ceremony. Prod call sites always inject options via operator.Runtime, so this default
// only affects call paths that never had a chance to reach the gate-ON branch.
func podDeletionCostManagementEnabled(ctx context.Context) bool {
	if !options.HasContext(ctx) {
		return false
	}
	return options.FromContext(ctx).FeatureGates.PodDeletionCostManagement
}

func IsUnderConsolidateAfter(nodePool *v1.NodePool, nodeClaim *v1.NodeClaim, c clock.Clock) bool {
	if nodePool == nil || nodeClaim == nil || nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil || lo.FromPtr(nodePool.Spec.Disruption.ConsolidateAfter.Duration) == 0 {
		return false
	}
	if !nodeClaim.StatusConditions().IsTrue(v1.ConditionTypeInitialized) {
		return false
	}
	initialized := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)

	// If the lastPodEvent is zero, use the time that the nodeclaim was initialized, as that's when Karpenter recognizes that pods could have started scheduling
	timeToCheck := lo.Ternary(!nodeClaim.Status.LastPodEventTime.IsZero(), nodeClaim.Status.LastPodEventTime.Time, initialized.LastTransitionTime.Time)

	// Consider a node under the effect of consolidateAfter by looking at the lastPodEvent status field on the nodeclaim.
	return c.Since(timeToCheck) < lo.FromPtr(nodePool.Spec.Disruption.ConsolidateAfter.Duration)
}
