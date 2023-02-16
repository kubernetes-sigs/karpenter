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

package node

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
)

type Drift struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func (d *Drift) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner, node *v1.Node) (reconcile.Result, error) {
	if _, ok := node.Annotations[v1alpha5.VoluntaryDisruptionAnnotationKey]; ok {
		return reconcile.Result{}, nil
	}
	machineName, ok := node.Labels[v1alpha5.MachineNameLabelKey]
	if !ok {
		return reconcile.Result{}, nil
	}
	machine := &v1alpha5.Machine{}
	if err := d.kubeClient.Get(ctx, types.NamespacedName{Name: machineName}, machine); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !machine.StatusConditions().GetCondition(v1alpha5.MachineCreated).IsTrue() {
		return reconcile.Result{Requeue: true}, nil
	}
	// TODO: Add Provisioner Drift
	drifted, err := d.cloudProvider.IsMachineDrifted(ctx, machine)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting drift for node, %w", err)
	}
	if !drifted {
		// Requeue after 5 minutes for the cache TTL
		return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	node.Annotations = lo.Assign(node.Annotations, map[string]string{
		v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionDriftedAnnotationValue,
	})
	if err = d.kubeClient.Update(ctx, node); err != nil {
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	logging.FromContext(ctx).Debugf("node drifted")
	// Requeue after 5 minutes for the cache TTL
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}
