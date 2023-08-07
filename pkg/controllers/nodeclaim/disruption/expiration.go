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

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

// Expiration is a machine sub-controller that adds or removes status conditions on expired machines based on TTLSecondsUntilExpired
type Expiration struct {
	kubeClient client.Client
	clock      clock.Clock
}

//nolint:gocyclo
func (e *Expiration) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	hasExpiredCondition := nodeClaim.StatusConditions().GetCondition(v1beta1.NodeExpired) != nil

	// From here there are three scenarios to handle:
	// 1. If ExpirationTTL is not configured, remove the expired status condition
	if nodePool.Spec.Deprovisioning.ExpirationTTL.Duration < 0 {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.NodeExpired)
		if hasExpiredCondition {
			logging.FromContext(ctx).Debugf("removing expiration status condition since expiration has been disabled")
		}
		return reconcile.Result{}, nil
	}
	expired := nodeclaimutil.IsExpired(nodeClaim, e.clock, nodePool)
	expirationTime := nodeclaimutil.GetExpirationTime(nodeClaim, nodePool)
	// 2. If the nodeClaim isn't expired, remove the status condition.
	if !expired {
		_ = nodeClaim.StatusConditions().ClearCondition(v1alpha5.MachineExpired)
		if hasExpiredCondition {
			logging.FromContext(ctx).Debugf("removing expired status condition since not expired")
		}
		// If the nodeClaim isn't expired and doesn't have the status condition, return.
		// Use t.Sub(clock.Now()) instead of time.Until() to ensure we're using the injected clock.
		return reconcile.Result{RequeueAfter: expirationTime.Sub(e.clock.Now())}, nil
	}
	// 3. Otherwise, if the nodeClaim is expired, but doesn't have the status condition, add it.
	nodeClaim.StatusConditions().SetCondition(apis.Condition{
		Type:     v1beta1.NodeExpired,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
	})
	if !hasExpiredCondition {
		logging.FromContext(ctx).Debugf("marking as expired")
	}

	return reconcile.Result{}, nil
}
