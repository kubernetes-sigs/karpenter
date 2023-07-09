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
	"k8s.io/utils/clock"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	"github.com/aws/karpenter-core/pkg/utils/node"
)

// Emptiness is a machine sub-controller that adds or removes status conditions on empty machines based on TTLSecondsAfterEmpty
type Emptiness struct {
	kubeClient client.Client
	cluster    *state.Cluster
	clock      clock.Clock
}

//nolint:gocyclo
func (e *Emptiness) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner, machine *v1alpha5.Machine) (reconcile.Result, error) {
	hasEmptyCondition := machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty) != nil

	// From here there are three scenarios to handle:
	// 1. If TTLSecondsAfterEmpty is not configured, remove the emptiness status condition
	if provisioner.Spec.TTLSecondsAfterEmpty == nil {
		if hasEmptyCondition {
			_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineEmpty)
			logging.FromContext(ctx).Debugf("removing emptiness status condition from machine since emptiness is disabled")
		}
		return reconcile.Result{}, nil
	}
	// 2. If Machine is not initialized, remove the emptiness status condition
	if initCond := machine.StatusConditions().GetCondition(v1alpha5.MachineInitialized); initCond == nil || initCond.IsFalse() {
		if hasEmptyCondition {
			_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineEmpty)
			logging.FromContext(ctx).Debugf("removing emptiness status condition from machine since machine isn't initialized")
		}
		return reconcile.Result{}, nil
	}
	// Get the node to check for pods scheduled to it
	n, err := machineutil.NodeForMachine(ctx, e.kubeClient, machine)
	if err != nil {
		// 3. If Machine -> Node mapping doesn't exist, remove the emptiness status condition
		if machineutil.IsDuplicateNodeError(err) || machineutil.IsNodeNotFoundError(err) {
			_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineEmpty)
			if hasEmptyCondition {
				logging.FromContext(ctx).Debugf("removing emptiness status condition from machine since machine doesn't have a single node mapping")
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	// Node is empty, but it is in-use per the last scheduling round, so we don't consider it empty
	// We perform a short requeue if the node is nominated, so we can check the node for emptiness when the node
	// nomination time ends since we don't watch node nomination events
	// 4. If the Node is nominated for pods to schedule to it, remove the emptiness status condition
	if e.cluster.IsNodeNominated(n.Name) {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineEmpty)
		if hasEmptyCondition {
			logging.FromContext(ctx).Debugf("removing emptiness status condition from machine since machine is nominated for pods")
		}
		return reconcile.Result{RequeueAfter: time.Second * 30}, nil
	}
	pods, err := node.GetNodePods(ctx, e.kubeClient, n)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("retrieving node pods, %w", err)
	}
	// 5. If there are pods that are actively scheduled to the Node, remove the emptiness status condition
	if len(pods) > 0 {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineEmpty)
		if hasEmptyCondition {
			logging.FromContext(ctx).Debugf("removing emptiness status condition from node")
		}
		return reconcile.Result{}, nil
	}
	// 6. Otherwise, add the emptiness status condition
	machine.StatusConditions().SetCondition(apis.Condition{
		Type:     v1alpha5.MachineEmpty,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
	})
	if !hasEmptyCondition {
		logging.FromContext(ctx).Debugf("marking machine as empty")
	}
	return reconcile.Result{}, nil
}
