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

	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

// Expiration is a node sub-controller that annotates or de-annotates an expired node based on TTLSecondsUntilExpired
type Expiration struct {
	kubeClient client.Client
	clock      clock.Clock
}

//nolint:gocyclo
func (e *Expiration) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner, machine *v1alpha5.Machine) (reconcile.Result, error) {
	hasExpiredCondition := machine.StatusConditions().GetCondition(v1alpha5.MachineExpired).IsTrue()

	// From here there are three scenarios to handle:
	// 1. If TTLSecondsUntilExpired is not configured, but the node is expired,
	//    remove the annotation so another disruption controller can annotate the node.
	if provisioner.Spec.TTLSecondsUntilExpired == nil {
		if hasExpiredCondition {
			_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineExpired)
			logging.FromContext(ctx).Debugf("removing expiration status condition from machine as expiration has been disabled")
		}
		return reconcile.Result{}, nil
	}
	node, err := machineutil.NodeForMachine(ctx, e.kubeClient, machine)
	if machineutil.IgnoreNodeNotFoundError(err) != nil {
		return reconcile.Result{}, err
	}
	// We do the expiration check in this way since there is still a migration path for creating Machines from Nodes
	// In this case, we need to make sure that we take the older of the two for expiration
	var expired bool
	if node == nil || machine.CreationTimestamp.Before(&node.CreationTimestamp) {
		expired = functional.IsExpired(machine, e.clock, provisioner)
	} else {
		expired = functional.IsExpired(node, e.clock, provisioner)
	}

	// 2. Otherwise, if the node is expired, but doesn't have the status condition, add it.
	if expired && !hasExpiredCondition {
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineExpired, "ExpirationTTLExceeded", "")
		logging.FromContext(ctx).Debugf("marking machine as expired")
		return reconcile.Result{}, nil
	}
	// 3. Finally, if the node isn't expired, but has the status condition, remove it.
	if !expired && hasExpiredCondition {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineExpired)
		logging.FromContext(ctx).Debugf("removing expired status condition from machine")
	}
	// If the node isn't expired and doesn't have the status condition, return.
	// Use t.Sub(time.Now()) instead of time.Until() to ensure we're using the injected clock.
	return reconcile.Result{RequeueAfter: functional.GetExpirationTime(machine, provisioner).Sub(e.clock.Now())}, nil
}
