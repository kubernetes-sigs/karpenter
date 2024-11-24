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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	"sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

type DrainReconciler struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	recorder      events.Recorder
	terminator    *terminator.Terminator
}

// nolint:gocyclo
func (d *DrainReconciler) Reconcile(ctx context.Context, n *corev1.Node, nc *v1.NodeClaim) (reconcile.Result, error) {
	if nc.StatusConditions().IsTrue(v1.ConditionTypeDrained) {
		return reconcile.Result{}, nil
	}

	tgpExpirationTime, err := nodeclaim.TerminationGracePeriodExpirationTime(nc)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to terminate node")
		return reconcile.Result{}, nil
	}
	if tgpExpirationTime != nil {
		d.recorder.Publish(terminatorevents.NodeTerminationGracePeriodExpiring(n, *tgpExpirationTime))
	}

	if err := d.terminator.Drain(ctx, n, tgpExpirationTime); err != nil {
		if !terminator.IsNodeDrainError(err) {
			return reconcile.Result{}, fmt.Errorf("draining node, %w", err)
		}
		// If the underlying NodeClaim no longer exists, we want to delete to avoid trying to gracefully draining
		// on nodes that are no longer alive. We do a check on the Ready condition of the node since, even
		// though the CloudProvider says the instance is not around, we know that the kubelet process is still running
		// if the Node Ready condition is true
		// Similar logic to: https://github.com/kubernetes/kubernetes/blob/3a75a8c8d9e6a1ebd98d8572132e675d4980f184/staging/src/k8s.io/cloud-provider/controllers/nodelifecycle/node_lifecycle_controller.go#L144
		if nodeutils.GetCondition(n, corev1.NodeReady).Status != corev1.ConditionTrue {
			if _, err = d.cloudProvider.Get(ctx, n.Spec.ProviderID); err != nil {
				if cloudprovider.IsNodeClaimNotFoundError(err) {
					return reconcile.Result{}, removeFinalizer(ctx, d.kubeClient, n)
				}
				return reconcile.Result{}, fmt.Errorf("getting nodeclaim, %w", err)
			}
		}
		stored := nc.DeepCopy()
		if nc.StatusConditions().SetFalse(v1.ConditionTypeDrained, "Draining", "Draining") {
			if err := d.kubeClient.Status().Patch(ctx, nc, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
				if errors.IsNotFound(err) {
					return reconcile.Result{}, nil
				}
				if errors.IsConflict(err) {
					return reconcile.Result{Requeue: true}, nil
				}
				return reconcile.Result{}, err
			}
		}
		d.recorder.Publish(terminatorevents.NodeFailedToDrain(n, err))
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}

	stored := nc.DeepCopy()
	_ = nc.StatusConditions().SetTrue(v1.ConditionTypeDrained)
	if err := d.kubeClient.Status().Patch(ctx, nc, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	NodesDrainedTotal.Inc(map[string]string{
		metrics.NodePoolLabel: n.Labels[v1.NodePoolLabelKey],
	})
	return reconcile.Result{}, nil
}
