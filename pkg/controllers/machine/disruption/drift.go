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

	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
)

type Drift struct {
	cloudProvider cloudprovider.CloudProvider
}

func (d *Drift) Reconcile(ctx context.Context, _ *v1alpha5.Provisioner, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if !machine.StatusConditions().GetCondition(v1alpha5.MachineLaunched).IsTrue() {
		return reconcile.Result{}, nil
	}
	// If the machine is marked as voluntarily disrupted by another controller, do nothing.
	voluntarilyDisrupted := machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)
	if voluntarilyDisrupted.IsTrue() && voluntarilyDisrupted.Reason != v1alpha5.VoluntarilyDisruptedReasonDrifted {
		return reconcile.Result{}, nil
	}
	hasDriftedCondition := voluntarilyDisrupted.IsTrue()

	// From here there are three scenarios to handle:
	// 1. If drift is not enabled but the node is drifted, remove the annotation
	//    so another disruption controller can annotate the node.
	if !settings.FromContext(ctx).DriftEnabled {
		if hasDriftedCondition {
			_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineVoluntarilyDisrupted)
			logging.FromContext(ctx).Debugf("removing drift status condition from machine as drift has been disabled")
		}
		return reconcile.Result{}, nil
	}

	drifted, err := d.cloudProvider.IsMachineDrifted(ctx, machine)
	if err != nil {
		return reconcile.Result{}, cloudprovider.IgnoreMachineNotFoundError(fmt.Errorf("getting drift for machine, %w", err))
	}
	// 2. Otherwise, if the node isn't drifted, but has the annotation, remove it.
	if !drifted && hasDriftedCondition {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineVoluntarilyDisrupted)
		logging.FromContext(ctx).Debugf("removing drifted status condition from machine")
		// 3. Finally, if the node is drifted, but doesn't have the annotation, add it.
	} else if drifted && !hasDriftedCondition {
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineVoluntarilyDisrupted, v1alpha5.VoluntarilyDisruptedReasonDrifted, "")
		logging.FromContext(ctx).Debugf("marking machine as drifted")
	}
	// Requeue after 5 minutes for the cache TTL
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}
