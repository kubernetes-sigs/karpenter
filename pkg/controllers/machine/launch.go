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
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
)

type Launch struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func (l *Launch) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if machine.Status.ProviderID != "" {
		return reconcile.Result{}, nil
	}
	updated, err := l.cloudProvider.Get(ctx, machine.Name, machine.Labels[v1alpha5.ProvisionerNameLabelKey])
	if err != nil {
		if cloudprovider.IsMachineNotFoundError(err) {
			logging.FromContext(ctx).Debugf("creating machine")
			_, err = l.cloudProvider.Create(ctx, machine)
			if err != nil {
				return reconcile.Result{}, fmt.Errorf("creating machine, %w", err)
			}
		} else {
			return reconcile.Result{}, fmt.Errorf("getting machine, %w", err)
		}
	}
	if updated == nil {
		return reconcile.Result{Requeue: true}, nil
	}
	populateMachineDetails(machine, updated)
	machine.StatusConditions().MarkTrue(v1alpha5.MachineCreated)
	return reconcile.Result{}, nil
}

func populateMachineDetails(machine, retrieved *v1alpha5.Machine) {
	machine.Labels = lo.Assign(machine.Labels, retrieved.Labels, map[string]string{
		v1alpha5.MachineNameLabelKey: machine.Name,
	})
	machine.Annotations = lo.Assign(machine.Annotations, retrieved.Annotations)
	machine.Status.ProviderID = retrieved.Status.ProviderID
	machine.Status.Allocatable = retrieved.Status.Allocatable
	machine.Status.Capacity = retrieved.Status.Capacity
}
