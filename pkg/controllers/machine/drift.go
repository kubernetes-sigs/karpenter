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
	"time"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

type Drift struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func (d *Drift) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if !settings.FromContext(ctx).DriftEnabled {
		return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	node, err := machineutil.NodeForMachine(ctx, d.kubeClient, machine)
	if err != nil {
		return reconcile.Result{}, nil //nolint:nilerr
	}
	if _, ok := node.Annotations[v1alpha5.VoluntaryDisruptionAnnotationKey]; ok {
		return reconcile.Result{}, nil
	}
	// TODO: Add Provisioner Drift
	drifted, err := d.cloudProvider.IsMachineDrifted(ctx, machine)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting drift for node, %w", err)
	}
	if drifted {
		stored := node.DeepCopy()
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionDriftedAnnotationValue,
		})
		if !equality.Semantic.DeepEqual(stored, node) {
			if err = d.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
				return reconcile.Result{}, err
			}
		}
	}
	// Requeue after 5 minutes for the cache TTL
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}
