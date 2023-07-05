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
	"github.com/aws/karpenter-core/pkg/cloudprovider"
)

// Drift is a machine sub-controller that adds or removes status conditions on drifted machines
type Drift struct {
	cloudProvider cloudprovider.CloudProvider
}

func (d *Drift) Reconcile(ctx context.Context, _ *v1alpha5.Provisioner, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if !machine.StatusConditions().GetCondition(v1alpha5.MachineLaunched).IsTrue() {
		return reconcile.Result{}, nil
	}
	hasDriftedCondition := machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()
	// From here there are three scenarios to handle:
	// 1. If drift is not enabled but the machine is drifted, remove the status condition
	if !settings.FromContext(ctx).DriftEnabled {
		if hasDriftedCondition {
			_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineDrifted)
			logging.FromContext(ctx).Debugf("removing drift status condition from machine as drift has been disabled")
		}
		return reconcile.Result{}, nil
	}

	drifted, err := d.cloudProvider.IsMachineDrifted(ctx, machine)
	if err != nil {
		return reconcile.Result{}, cloudprovider.IgnoreMachineNotFoundError(fmt.Errorf("getting drift for machine, %w", err))
	}
	// 2. Otherwise, if the machine isn't drifted, but has the annotation, remove it.
	if !drifted && hasDriftedCondition {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineDrifted)
		logging.FromContext(ctx).Debugf("removing drifted status condition from machine")
		// 3. Finally, if the machine is drifted, but doesn't have the annotation, add it.
	} else if drifted && !hasDriftedCondition {
		machine.StatusConditions().SetCondition(apis.Condition{
			Type:     v1alpha5.MachineDrifted,
			Status:   v1.ConditionTrue,
			Reason:   "NodeTemplateDrifted",
			Severity: apis.ConditionSeverityWarning,
		})
		logging.FromContext(ctx).Debugf("marking machine as drifted")
	}
	// Requeue after 5 minutes for the cache TTL
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}
