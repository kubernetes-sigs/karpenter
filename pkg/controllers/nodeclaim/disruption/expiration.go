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
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/metrics"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

// Expiration is a machine sub-controller that adds or removes status conditions on expired machines based on TTLSecondsUntilExpired
type Expiration struct {
	kubeClient client.Client
	clock      clock.Clock
}

//nolint:gocyclo
func (e *Expiration) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	hasExpiredCondition := nodeClaim.StatusConditions().GetCondition(v1beta1.Expired) != nil

	// From here there are three scenarios to handle:
	// 1. If ExpireAfter is not configured, remove the expired status condition
	if nodePool.Spec.Disruption.ExpireAfter.Duration == nil {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Expired)
		if hasExpiredCondition {
			logging.FromContext(ctx).Debugf("removing expiration status condition, expiration has been disabled")
		}
		return reconcile.Result{}, nil
	}
	node, err := nodeclaimutil.NodeForNodeClaim(ctx, e.kubeClient, nodeClaim)
	if nodeclaimutil.IgnoreNodeNotFoundError(nodeclaimutil.IgnoreDuplicateNodeError(err)) != nil {
		return reconcile.Result{}, err
	}
	// We do the expiration check in this way since there is still a migration path for creating Machines from Nodes
	// In this case, we need to make sure that we take the older of the two for expiration
	// TODO @joinnis: This check that takes the minimum between the Node and Machine CreationTimestamps can be removed
	// once machine migration is ripped out, which should happen when apis and Karpenter are promoted to v1
	var expirationTime time.Time
	if node == nil || nodeClaim.CreationTimestamp.Before(&node.CreationTimestamp) {
		expirationTime = nodeClaim.CreationTimestamp.Add(*nodePool.Spec.Disruption.ExpireAfter.Duration)
	} else {
		expirationTime = node.CreationTimestamp.Add(*nodePool.Spec.Disruption.ExpireAfter.Duration)
	}
	// 2. If the NodeClaim isn't expired, remove the status condition.
	if e.clock.Now().Before(expirationTime) {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Expired)
		if hasExpiredCondition {
			logging.FromContext(ctx).Debugf("removing expired status condition, not expired")
		}
		// If the NodeClaim isn't expired and doesn't have the status condition, return.
		// Use t.Sub(clock.Now()) instead of time.Until() to ensure we're using the injected clock.
		return reconcile.Result{RequeueAfter: expirationTime.Sub(e.clock.Now())}, nil
	}
	// 3. Otherwise, if the NodeClaim is expired, but doesn't have the status condition, add it.
	nodeClaim.StatusConditions().SetCondition(apis.Condition{
		Type:     v1beta1.Expired,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
	})
	if !hasExpiredCondition {
		logging.FromContext(ctx).Debugf("marking expired")
		nodeclaimutil.DisruptedCounter(nodeClaim, metrics.ExpirationReason).Inc()
	}
	return reconcile.Result{}, nil
}
