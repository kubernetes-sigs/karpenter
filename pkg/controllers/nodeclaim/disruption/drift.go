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
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	NodePoolDrifted     cloudprovider.DriftReason = "NodePoolDrifted"
	RequirementsDrifted cloudprovider.DriftReason = "RequirementsDrifted"
)

// Drift is a nodeclaim sub-controller that adds or removes status conditions on drifted nodeclaims
type Drift struct {
	cloudProvider cloudprovider.CloudProvider
}

func (d *Drift) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	hasDriftedCondition := nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted) != nil

	// From here there are three scenarios to handle:
	// 1. If drift is not enabled but the NodeClaim is drifted, remove the status condition
	if !options.FromContext(ctx).FeatureGates.Drift {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Drifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drift status condition, drift has been disabled")
		}
		return reconcile.Result{}, nil
	}
	// 2. If NodeClaim is not launched, remove the drift status condition
	if launchCond := nodeClaim.StatusConditions().GetCondition(v1beta1.Launched); launchCond == nil || launchCond.IsFalse() {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Drifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drift status condition, isn't launched")
		}
		return reconcile.Result{}, nil
	}
	driftedReason, err := d.isDrifted(ctx, nodePool, nodeClaim)
	if err != nil {
		return reconcile.Result{}, cloudprovider.IgnoreNodeClaimNotFoundError(fmt.Errorf("getting drift, %w", err))
	}
	// 3. Otherwise, if the NodeClaim isn't drifted, but has the status condition, remove it.
	if driftedReason == "" {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Drifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drifted status condition, not drifted")
		}
		return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	// 4. Finally, if the NodeClaim is drifted, but doesn't have status condition, add it.
	nodeClaim.StatusConditions().SetCondition(apis.Condition{
		Type:     v1beta1.Drifted,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
		Reason:   string(driftedReason),
	})
	if !hasDriftedCondition {
		logging.FromContext(ctx).With("reason", string(driftedReason)).Debugf("marking drifted")
		metrics.NodeClaimsDisruptedCounter.With(prometheus.Labels{
			metrics.TypeLabel:     metrics.DriftReason,
			metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
		}).Inc()
		metrics.NodeClaimsDriftedCounter.With(prometheus.Labels{
			metrics.TypeLabel:     string(driftedReason),
			metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
		}).Inc()
	}
	// Requeue after 5 minutes for the cache TTL
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

// isDrifted will check if a NodeClaim is drifted from the fields in the NodePool Spec and the CloudProvider
func (d *Drift) isDrifted(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (cloudprovider.DriftReason, error) {
	// First check for static drift or node requirements have drifted to save on API calls.
	if reason := lo.FindOrElse([]cloudprovider.DriftReason{areStaticFieldsDrifted(nodePool, nodeClaim), areRequirementsDrifted(nodePool, nodeClaim)}, "", func(i cloudprovider.DriftReason) bool {
		return i != ""
	}); reason != "" {
		return reason, nil
	}
	driftedReason, err := d.cloudProvider.IsDrifted(ctx, nodeClaim)
	if err != nil {
		return "", err
	}
	return driftedReason, nil
}

// Eligible fields for drift are described in the docs
// https://karpenter.sh/docs/concepts/deprovisioning/#drift
func areStaticFieldsDrifted(nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) cloudprovider.DriftReason {
	nodePoolHash, foundHashNodePool := nodePool.Annotations[v1beta1.NodePoolHashAnnotationKey]
	nodePoolVersionHash, foundVersionHashNodePool := nodePool.Annotations[v1beta1.NodePoolHashVersionAnnotationKey]
	nodeClaimHash, foundHashNodeClaim := nodeClaim.Annotations[v1beta1.NodePoolHashAnnotationKey]
	nodeClaimVersionHash, foundVersionHashNodeClaim := nodeClaim.Annotations[v1beta1.NodePoolHashVersionAnnotationKey]

	if !foundHashNodePool || !foundHashNodeClaim || !foundVersionHashNodePool || !foundVersionHashNodeClaim {
		return ""
	}
	// validate that the version of the crd is the same
	if nodePoolVersionHash != nodeClaimVersionHash {
		return ""
	}
	return lo.Ternary(nodePoolHash != nodeClaimHash, NodePoolDrifted, "")
}

func areRequirementsDrifted(nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) cloudprovider.DriftReason {
	nodepoolReq := scheduling.NewNodeSelectorRequirementsWithMinValues(nodePool.Spec.Template.Spec.Requirements...)
	nodeClaimReq := scheduling.NewLabelRequirements(nodeClaim.Labels)

	// Every nodepool requirement is compatible with the NodeClaim label set
	if nodeClaimReq.Compatible(nodepoolReq) != nil {
		return RequirementsDrifted
	}

	return ""
}
