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

package notready

import (
	"context"
	"time"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nodeclaimutil "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Controller NotReady is a nodeclaim controller that deletes nodeclaims when they have been unreachable for too long
type Controller struct {
	kubeClient client.Client
}

// NewController constructs a nodeclaim disruption controller
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	if nodeClaim.Spec.UnreachableTimeout.Duration == nil {
		return reconcile.Result{}, nil
	}

	node, err := nodeclaimutil.NodeForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		return reconcile.Result{}, nodeclaimutil.IgnoreDuplicateNodeError(nodeclaimutil.IgnoreNodeNotFoundError(err))
	}

	for _, taint := range node.Spec.Taints {
		if taint.Key == corev1.TaintNodeUnreachable {
			if taint.TimeAdded != nil {
				durationSinceTaint := time.Since(taint.TimeAdded.Time)
				if durationSinceTaint > *nodeClaim.Spec.UnreachableTimeout.Duration {
					// if node is unreachable for too long, delete the nodeclaim
					if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
						log.FromContext(ctx).V(0).Error(err, "Failed to delete NodeClaim", "node", node.Name)
						return reconcile.Result{}, err
					}
					log.FromContext(ctx).V(0).Info("Deleted NodeClaim because the node has been unreachable for more than unreachableTimeout", "node", node.Name)
					return reconcile.Result{}, nil
				} else {
					// If the node is unreachable and the time since it became unreachable is less than the configured timeout,
					// we requeue to prevent the node from remaining in an unreachable state indefinitely
					log.FromContext(ctx).V(1).Info("Node has been unreachable for less than unreachableTimeout, requeueing", "node", node.Name)
					return reconcile.Result{RequeueAfter: *nodeClaim.Spec.UnreachableTimeout.Duration}, nil
				}
			}
		}
	}

	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	builder := controllerruntime.NewControllerManagedBy(m)
	return builder.
		Named("nodeclaim.notready").
		For(&v1.NodeClaim{}).
		Watches(
			&corev1.Node{},
			nodeclaimutil.NodeEventHandler(c.kubeClient),
		).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
