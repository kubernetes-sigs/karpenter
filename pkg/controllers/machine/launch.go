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

	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

type Launch struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cache         *cache.Cache // exists due to eventual consistency on the cache
}

func (l *Launch) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if machine.Status.ProviderID != "" {
		return reconcile.Result{}, nil
	}
	var err error
	var created *v1alpha5.Machine
	if ret, ok := l.cache.Get(client.ObjectKeyFromObject(machine).String()); ok {
		created = ret.(*v1alpha5.Machine)
	} else if id, ok := machine.Annotations[v1alpha5.MachineLinkedAnnotationKey]; ok {
		created, err = l.cloudProvider.Get(ctx, id)
		if err != nil {
			if cloudprovider.IsMachineNotFoundError(err) {
				if err = l.kubeClient.Delete(ctx, machine); err != nil {
					return reconcile.Result{}, client.IgnoreNotFound(err)
				}
				logging.FromContext(ctx).Debugf("garbage collected machine with no cloudprovider representation")
				metrics.MachinesTerminatedCounter.With(prometheus.Labels{
					metrics.ReasonLabel:      "garbage_collected",
					metrics.ProvisionerLabel: machine.Labels[v1alpha5.ProvisionerNameLabelKey],
				})
				return reconcile.Result{}, nil
			}
			machine.StatusConditions().MarkFalse(v1alpha5.MachineCreated, "LinkFailed", truncateMessage(err.Error()))
			return reconcile.Result{}, fmt.Errorf("linking machine, %w", err)
		}
		logging.FromContext(ctx).Debugf("linked machine")
	} else {
		created, err = l.cloudProvider.Create(ctx, machine)
		if err != nil {
			if cloudprovider.IsInsufficientCapacityError(err) {
				logging.FromContext(ctx).Error(err)
				return reconcile.Result{}, client.IgnoreNotFound(l.kubeClient.Delete(ctx, machine))
			}
			machine.StatusConditions().MarkFalse(v1alpha5.MachineCreated, "LaunchFailed", truncateMessage(err.Error()))
			return reconcile.Result{}, fmt.Errorf("creating machine, %w", err)
		}
		logging.FromContext(ctx).Debugf("created machine")
	}
	l.cache.SetDefault(client.ObjectKeyFromObject(machine).String(), created)
	PopulateMachineDetails(machine, created)
	machine.StatusConditions().MarkTrue(v1alpha5.MachineCreated)
	return reconcile.Result{}, nil
}

func PopulateMachineDetails(machine, retrieved *v1alpha5.Machine) {
	machine.Labels = lo.Assign(
		scheduling.NewNodeSelectorRequirements(machine.Spec.Requirements...).Labels(),
		machine.Labels,
		retrieved.Labels,
		map[string]string{
			v1alpha5.MachineNameLabelKey: machine.Name,
		},
	)
	machine.Annotations = lo.Assign(machine.Annotations, retrieved.Annotations)
	machine.Status.ProviderID = retrieved.Status.ProviderID
	machine.Status.Allocatable = retrieved.Status.Allocatable
	machine.Status.Capacity = retrieved.Status.Capacity
}

func truncateMessage(msg string) string {
	if len(msg) < 200 {
		return msg
	}
	return msg[:200] + "..."
}
