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
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	nodeclaimutil "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// Consolidation is a nodeclaim sub-controller that adds or removes status conditions on nodeclaims when using WhenUnderutilized policy.
type Consolidation struct {
	kubeClient client.Client
	cluster    *state.Cluster
	clock      clock.Clock
}

//nolint:gocyclo
func (e *Consolidation) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	hasCondition := nodeClaim.StatusConditions().GetCondition(v1beta1.Consolidated) != nil
	logging.FromContext(ctx).Infof("Consolidate nodeclaim %s %v", nodeClaim.Name, hasCondition)

	if nodePool.Spec.Disruption.ConsolidationPolicy != v1beta1.ConsolidationPolicyWhenUnderutilized {
		if hasCondition {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Consolidated)
			logging.FromContext(ctx).Debugf("removing consolidated status condition, emptiness is disabled")
		}
		return reconcile.Result{}, nil
	}
	if initCond := nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized); initCond == nil || initCond.IsFalse() {
		if hasCondition {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Consolidated)
			logging.FromContext(ctx).Debugf("removing consolidated status condition, isn't initialized")
		}
		return reconcile.Result{}, nil
	}
	_, err := nodeclaimutil.NodeForNodeClaim(ctx, e.kubeClient, nodeClaim)
	if err != nil {
		if nodeclaimutil.IsDuplicateNodeError(err) || nodeclaimutil.IsNodeNotFoundError(err) {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Consolidated)
			if hasCondition {
				logging.FromContext(ctx).Debugf("removing consolidated status condition, doesn't have a single node mapping")
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	nodeClaim.StatusConditions().SetCondition(apis.Condition{
		Type:     v1beta1.Consolidated,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
	})
	logging.FromContext(ctx).Infof("update nodeclaim %s status", nodeClaim.Name)
	if !hasCondition {
		logging.FromContext(ctx).Debugf("marking consolidated")
		metrics.NodeClaimsDisruptedCounter.With(prometheus.Labels{
			metrics.TypeLabel:     metrics.ConsolidationReason,
			metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
		}).Inc()
	}
	return reconcile.Result{}, nil
}
