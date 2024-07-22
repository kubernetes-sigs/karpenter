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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	nodeclaimutil "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// Underutilization is a nodeclaim sub-controller that adds or removes status conditions on empty nodeclaims based on TTLSecondsAfterUnderutilized
type Underutilization struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

//nolint:gocyclo
func (e *Underutilization) Reconcile(ctx context.Context, nodePool *v1.NodePool, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	hasUnderutilizedCondition := nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized) != nil

	// 1. If ConsolidationPolicyWhenUnderutilized is not configured or ConsolidateAfter isn't configured, remove the underutilization status condition
	if nodePool.Spec.Disruption.ConsolidationPolicy != v1.ConsolidationPolicyWhenUnderutilized ||
		nodePool.Spec.Disruption.ConsolidateAfter == nil ||
		nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		if hasUnderutilizedCondition {
			_ = nodeClaim.StatusConditions().Clear(v1.ConditionTypeUnderutilized)
			log.FromContext(ctx).V(1).Info("removing underutilization status condition, underutilization is disabled")
		}
		return reconcile.Result{}, nil
	}
	// 2. If NodeClaim is not initialized, remove the underutilization status condition
	if !nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue() {
		if hasUnderutilizedCondition {
			_ = nodeClaim.StatusConditions().Clear(v1.ConditionTypeUnderutilized)
			log.FromContext(ctx).V(1).Info("removing underutilization status condition, isn't initialized")
		}
		return reconcile.Result{}, nil
	}
	// Get the node to check for pods scheduled to it
	n, err := nodeclaimutil.NodeForNodeClaim(ctx, e.kubeClient, nodeClaim)
	if err != nil {
		// 3. If Node mapping doesn't exist, remove the underutilization status condition
		if nodeclaimutil.IsDuplicateNodeError(err) || nodeclaimutil.IsNodeNotFoundError(err) {
			_ = nodeClaim.StatusConditions().Clear(v1.ConditionTypeUnderutilized)
			if hasUnderutilizedCondition {
				log.FromContext(ctx).V(1).Info("removing underutilization status condition, doesn't have a node mapping")
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// If node is not utilized, clear the condition, and requeue when we expect the node to be underutilized
	if underutilized, timeLeft := e.cluster.IsNodeUnderutilized(n.Spec.ProviderID, lo.FromPtr(nodePool.Spec.Disruption.ConsolidateAfter.Duration)); !underutilized {
		_ = nodeClaim.StatusConditions().Clear(v1.ConditionTypeUnderutilized)
		if hasUnderutilizedCondition {
			log.FromContext(ctx).V(1).Info("removing underutilization status condition, has recent pod churn")
		}
		return reconcile.Result{RequeueAfter: timeLeft}, nil
	}

	// 6. Otherwise, add the underutilization status condition
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
	if !hasUnderutilizedCondition {
		log.FromContext(ctx).V(1).Info("marking underutilized")

		metrics.NodeClaimsDisruptedCounter.With(prometheus.Labels{
			metrics.TypeLabel:     metrics.UnderutilizedReason,
			metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
		}).Inc()
	}
	return reconcile.Result{}, nil
}
