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

package lifecycle

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/metrics"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	nodeutil "github.com/aws/karpenter-core/pkg/utils/node"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

type Initialization struct {
	kubeClient client.Client
}

// Reconcile checks for initialization based on if:
// a) its current status is set to Ready
// b) all the startup taints have been removed from the node
// c) all extended resources have been registered
// This method handles both nil provisioners and nodes without extended resources gracefully.
func (i *Initialization) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if machine.StatusConditions().GetCondition(v1alpha5.MachineInitialized).IsTrue() {
		return reconcile.Result{}, nil
	}
	if !machine.StatusConditions().GetCondition(v1alpha5.MachineLaunched).IsTrue() {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "MachineNotLaunched", "Machine is not launched")
		return reconcile.Result{}, nil
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provider-id", machine.Status.ProviderID))
	node, err := machineutil.NodeForMachine(ctx, i.kubeClient, machine)
	if err != nil {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "NodeNotFound", "Node not registered with cluster")
		return reconcile.Result{}, nil //nolint:nilerr
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", node.Name))
	if nodeutil.GetCondition(node, v1.NodeReady).Status != v1.ConditionTrue {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "NodeNotReady", "Node status is NotReady")
		return reconcile.Result{}, nil
	}
	if taint, ok := IsStartupTaintRemoved(node, machine); !ok {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "StartupTaintsExist", "StartupTaint '%s' still exists", formatTaint(taint))
		return reconcile.Result{}, nil
	}
	if name, ok := RequestedResourcesRegistered(node, machine); !ok {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "ResourceNotRegistered", "Resource '%s' was requested but not registered", name)
		return reconcile.Result{}, nil
	}
	stored := node.DeepCopy()
	node.Labels = lo.Assign(node.Labels, map[string]string{v1alpha5.LabelNodeInitialized: "true"})
	if !equality.Semantic.DeepEqual(stored, node) {
		if err = i.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, err
		}
	}
	logging.FromContext(ctx).Debugf("initialized machine")
	machine.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
	metrics.MachinesInitializedCounter.With(prometheus.Labels{
		metrics.ProvisionerLabel: machine.Labels[v1alpha5.ProvisionerNameLabelKey],
	}).Inc()
	return reconcile.Result{}, nil
}

// IsStartupTaintRemoved returns true if there are no startup taints registered for the provisioner, or if all startup
// taints have been removed from the node
func IsStartupTaintRemoved(node *v1.Node, machine *v1alpha5.Machine) (*v1.Taint, bool) {
	if machine != nil {
		for _, startupTaint := range machine.Spec.StartupTaints {
			for i := range node.Spec.Taints {
				// if the node still has a startup taint applied, it's not ready
				if startupTaint.MatchTaint(&node.Spec.Taints[i]) {
					return &node.Spec.Taints[i], false
				}
			}
		}
	}
	return nil, true
}

// RequestedResourcesRegistered returns true if there are no extended resources on the node, or they have all been
// registered by device plugins
func RequestedResourcesRegistered(node *v1.Node, machine *v1alpha5.Machine) (v1.ResourceName, bool) {
	for resourceName, quantity := range machine.Spec.Resources.Requests {
		if quantity.IsZero() {
			continue
		}
		// kubelet will zero out both the capacity and allocatable for an extended resource on startup, so if our
		// annotation says the resource should be there, but it's zero'd in both then the device plugin hasn't
		// registered it yet.
		// We wait on allocatable since this is the value that is used in scheduling
		if resources.IsZero(node.Status.Allocatable[resourceName]) {
			return resourceName, false
		}
	}
	return "", true
}

func formatTaint(taint *v1.Taint) string {
	if taint == nil {
		return "<nil>"
	}
	if taint.Value == "" {
		return fmt.Sprintf("%s:%s", taint.Key, taint.Effect)
	}
	return fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect)
}
