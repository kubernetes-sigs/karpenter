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

package machine

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

type Registration struct {
	kubeClient client.Client
}

func (r *Registration) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if machine.Status.ProviderID == "" {
		return reconcile.Result{}, nil
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provider-id", machine.Status.ProviderID))
	node, err := nodeForMachine(ctx, r.kubeClient, machine)
	if err != nil {
		if IsNodeNotFoundError(err) {
			machine.StatusConditions().MarkFalse(v1alpha5.MachineRegistered, "NodeNotFound", "Machine not registered with cluster")
			return reconcile.Result{RequeueAfter: registrationTTL}, nil // Requeue later to check up to the registration timeout
		}
		if IsDuplicateNodeError(err) {
			machine.StatusConditions().MarkFalse(v1alpha5.MachineRegistered, "MultipleNodesFound", "Invariant violated, machine matched multiple nodes")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting node for machine, %w", err)
	}
	logging.WithLogger(ctx, logging.FromContext(ctx).With("node", node.Name))
	if err = r.syncNode(ctx, machine, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("syncing node, %w", err)
	}
	machine.StatusConditions().MarkTrue(v1alpha5.MachineRegistered)
	return reconcile.Result{}, nil
}

func (r *Registration) syncNode(ctx context.Context, machine *v1alpha5.Machine, node *v1.Node) error {
	stored := node.DeepCopy()
	controllerutil.AddFinalizer(node, v1alpha5.TerminationFinalizer)
	node.Labels = lo.Assign(node.Labels, machine.Labels)
	node.Annotations = lo.Assign(node.Annotations, machine.Annotations)

	// Sync all taints inside of Machine into the Machine taints
	node.Spec.Taints = scheduling.Taints(node.Spec.Taints).Merge(machine.Spec.Taints)
	if !machine.StatusConditions().GetCondition(v1alpha5.MachineRegistered).IsTrue() {
		node.Spec.Taints = scheduling.Taints(node.Spec.Taints).Merge(machine.Spec.StartupTaints)
	}
	lo.Must0(controllerutil.SetOwnerReference(machine, node, scheme.Scheme))
	if !equality.Semantic.DeepEqual(stored, node) {
		if err := r.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return fmt.Errorf("syncing node labels, %w", err)
		}
		logging.FromContext(ctx).Debugf("synced node")
	}
	return nil
}
