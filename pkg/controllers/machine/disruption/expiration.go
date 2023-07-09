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

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

// Expiration is a machine sub-controller that adds or removes status conditions on expired machines based on TTLSecondsUntilExpired
type Expiration struct {
	kubeClient client.Client
	clock      clock.Clock
}

//nolint:gocyclo
func (e *Expiration) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner, machine *v1alpha5.Machine) (reconcile.Result, error) {
	hasExpiredCondition := machine.StatusConditions().GetCondition(v1alpha5.MachineExpired) != nil

	// From here there are three scenarios to handle:
	// 1. If TTLSecondsUntilExpired is not configured, remove the expired status condition
	if provisioner.Spec.TTLSecondsUntilExpired == nil {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineExpired)
		if hasExpiredCondition {
			logging.FromContext(ctx).Debugf("removing expiration status condition from machine as expiration has been disabled")
		}
		return reconcile.Result{}, nil
	}
	node, err := machineutil.NodeForMachine(ctx, e.kubeClient, machine)
	if machineutil.IgnoreNodeNotFoundError(machineutil.IgnoreDuplicateNodeError(err)) != nil {
		return reconcile.Result{}, err
	}
	// We do the expiration check in this way since there is still a migration path for creating Machines from Nodes
	// In this case, we need to make sure that we take the older of the two for expiration
	// TODO @joinnis: This check that takes the minimum between the Node and Machine CreationTimestamps can be removed
	// once machine migration is ripped out, which should happen when apis and Karpenter are promoted to v1
	var expired bool
	var expirationTime time.Time
	if node == nil || machine.CreationTimestamp.Before(&node.CreationTimestamp) {
		expired = machineutil.IsExpired(machine, e.clock, provisioner)
		expirationTime = machineutil.GetExpirationTime(machine, provisioner)
	} else {
		expired = machineutil.IsExpired(node, e.clock, provisioner)
		expirationTime = machineutil.GetExpirationTime(machine, provisioner)
	}
	// 2. If the machine isn't expired, remove the status condition.
	if !expired {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineExpired)
		if hasExpiredCondition {
			logging.FromContext(ctx).Debugf("removing expired status condition from machine")
		}
		// If the machine isn't expired and doesn't have the status condition, return.
		// Use t.Sub(clock.Now()) instead of time.Until() to ensure we're using the injected clock.
		return reconcile.Result{RequeueAfter: expirationTime.Sub(e.clock.Now())}, nil
	}
	// 3. Otherwise, if the machine is expired, but doesn't have the status condition, add it.
	machine.StatusConditions().SetCondition(apis.Condition{
		Type:     v1alpha5.MachineExpired,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
	})
	if !hasExpiredCondition {
		logging.FromContext(ctx).Debugf("marking machine as expired")
	}

	return reconcile.Result{}, nil
}
