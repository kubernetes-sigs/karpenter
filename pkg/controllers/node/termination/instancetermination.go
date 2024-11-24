/*
Copyright The Kubernetes Authors.

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

package termination

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	"sigs.k8s.io/karpenter/pkg/utils/termination"
)

type InstanceTerminationReconciler struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	clk           clock.Clock
}

func (i *InstanceTerminationReconciler) Reconcile(ctx context.Context, n *corev1.Node, nc *v1.NodeClaim) (reconcile.Result, error) {
	elapsed, err := nodeclaimutils.HasTerminationGracePeriodElapsed(i.clk, nc)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to terminate node")
		return reconcile.Result{}, nil
	}
	if !nc.StatusConditions().Get(v1.ConditionTypeVolumesDetached).IsTrue() && !elapsed {
		return reconcile.Result{}, nil
	}
	isInstanceTerminated, err := termination.EnsureTerminated(ctx, i.kubeClient, nc, i.cloudProvider)
	if err != nil {
		// 404 = the nodeClaim no longer exists
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		// 409 - The nodeClaim exists, but its status has already been modified
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, fmt.Errorf("ensuring instance termination, %w", err)
	}
	if !isInstanceTerminated {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err := removeFinalizer(ctx, i.kubeClient, n); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}
