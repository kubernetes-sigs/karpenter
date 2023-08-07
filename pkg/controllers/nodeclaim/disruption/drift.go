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

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

// Drift is a nodeClaim sub-controller that adds or removes status conditions on drifted nodeClaims
type Drift struct {
	cloudProvider cloudprovider.CloudProvider
}

func (d *Drift) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	hasDriftedCondition := nodeClaim.StatusConditions().GetCondition(v1beta1.NodeDrifted) != nil

	// From here there are three scenarios to handle:
	// 1. If drift is not enabled but the nodeClaim is drifted, remove the status condition
	if !settings.FromContext(ctx).DriftEnabled {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.NodeDrifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drift status condition since drift has been disabled")
		}
		return reconcile.Result{}, nil
	}
	// 2. If NodeClaim is not launched, remove the drift status condition
	if !nodeClaim.StatusConditions().GetCondition(v1beta1.NodeLaunched).IsTrue() {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.NodeDrifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drift status condition since isn't launched")
		}
		return reconcile.Result{}, nil
	}
	drifted, err := d.isDrifted(ctx, nodePool, nodeClaim)
	if err != nil {
		return reconcile.Result{}, cloudprovider.IgnoreMachineNotFoundError(fmt.Errorf("getting drift for nodeClaim, %w", err))
	}
	// 3. Otherwise, if the nodeClaim isn't drifted, but has the status condition, remove it.
	if !drifted {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.NodeDrifted)
		if hasDriftedCondition {
			logging.FromContext(ctx).Debugf("removing drifted status condition since not drifted")
		}
		return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	// 4. Finally, if the nodeClaim is drifted, but doesn't have status condition, add it.
	nodeClaim.StatusConditions().SetCondition(apis.Condition{
		Type:     v1beta1.NodeDrifted,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
	})
	if !hasDriftedCondition {
		logging.FromContext(ctx).Debugf("marking as drifted")
	}
	// Requeue after 5 minutes for the cache TTL
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

// isDrifted will check if a machine is drifted from the fields in the provisioner.Spec and
// the cloudprovider
func (d *Drift) isDrifted(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (bool, error) {
	cloudProviderDrifted, err := d.cloudProvider.IsMachineDrifted(ctx, machineutil.NewFromNodeClaim(nodeClaim))
	if err != nil {
		return false, err
	}

	return cloudProviderDrifted || areStaticFieldsDrifted(nodePool, nodeClaim), nil
}

// Eligible fields for static drift are described in the docs
// https://karpenter.sh/docs/concepts/deprovisioning/#drift
// TODO @joinnis: Fix these annotations so that they reference the correct API version
func areStaticFieldsDrifted(nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) bool {
	provisionerHash, foundHashProvisioner := nodePool.Annotations[v1alpha5.ProvisionerHashAnnotationKey]
	machineHash, foundHashMachine := nodeClaim.Annotations[v1alpha5.ProvisionerHashAnnotationKey]
	if !foundHashProvisioner || !foundHashMachine {
		return false
	}
	return provisionerHash != machineHash
}
