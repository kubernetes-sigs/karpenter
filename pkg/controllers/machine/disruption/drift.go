/*
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

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

const (
	ProvisionerDrifted  cloudprovider.DriftReason = "ProvisionerDrifted"
	RequirementsDrifted cloudprovider.DriftReason = "RequirementsDrifted"
)

// Drift is a machine sub-controller that adds or removes status conditions on drifted machines
type Drift struct {
	cloudProvider cloudprovider.CloudProvider
}

func (d *Drift) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	hasDriftedCondition := nodeClaim.StatusConditions().GetCondition(v1beta1.NodeDrifted) != nil

	// From here there are three scenarios to handle:
	// 1. If drift is not enabled but the NodeClaim is drifted, remove the status condition
	if !settings.FromContext(ctx).DriftEnabled {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.NodeDrifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drift status condition, drift has been disabled")
		}
		return reconcile.Result{}, nil
	}
	// 2. If NodeClaim is not launched, remove the drift status condition
	if launchCond := nodeClaim.StatusConditions().GetCondition(v1beta1.NodeLaunched); launchCond == nil || launchCond.IsFalse() {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.NodeDrifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drift status condition, isn't launched")
		}
		return reconcile.Result{}, nil
	}
	driftedReason, err := d.isDrifted(ctx, nodePool, nodeClaim)
	if err != nil {
		return reconcile.Result{}, cloudprovider.IgnoreMachineNotFoundError(fmt.Errorf("getting drift, %w", err))
	}
	// 3. Otherwise, if the NodeClaim isn't drifted, but has the status condition, remove it.
	if driftedReason == "" {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.NodeDrifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drifted status condition, not drifted")
		}
		return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	// 4. Finally, if the NodeClaim is drifted, but doesn't have status condition, add it.
	nodeClaim.StatusConditions().SetCondition(apis.Condition{
		Type:     v1beta1.NodeDrifted,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
		Reason:   string(driftedReason),
	})
	if !hasDriftedCondition {
		logging.FromContext(ctx).Debugf("marking drifted")
		nodeclaimutil.DisruptedCounter(nodeClaim, metrics.DriftReason).Inc()
		nodeclaimutil.DriftedCounter(nodeClaim, string(driftedReason)).Inc()
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
	driftedReason, err := d.cloudProvider.IsMachineDrifted(ctx, machineutil.NewFromNodeClaim(nodeClaim))
	if err != nil {
		return "", err
	}
	return driftedReason, nil
}

// Eligible fields for static drift are described in the docs
// https://karpenter.sh/docs/concepts/deprovisioning/#drift
func areStaticFieldsDrifted(nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) cloudprovider.DriftReason {
	var ownerHashKey string
	if nodeClaim.IsMachine {
		ownerHashKey = v1alpha5.ProvisionerHashAnnotationKey
	} else {
		ownerHashKey = v1beta1.NodePoolHashAnnotationKey
	}
	nodePoolHash, foundHashNodePool := nodePool.Annotations[ownerHashKey]
	nodeClaimHash, foundHashNodeClaim := nodeClaim.Annotations[ownerHashKey]
	if !foundHashNodePool || !foundHashNodeClaim {
		return ""
	}
	if nodePoolHash != nodeClaimHash {
		return ProvisionerDrifted
	}
	return ""
}

func areRequirementsDrifted(nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) cloudprovider.DriftReason {
	provisionerReq := scheduling.NewNodeSelectorRequirements(nodePool.Spec.Template.Spec.Requirements...)
	nodeClaimReq := scheduling.NewLabelRequirements(nodeClaim.Labels)

	// Every provisioner requirement is compatible with the NodeClaim label set
	if nodeClaimReq.StrictlyCompatible(provisionerReq) != nil {
		return RequirementsDrifted
	}

	return ""
}
