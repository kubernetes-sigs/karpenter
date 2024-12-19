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

package readiness

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodepool.degraded")
	stored := nodePool.DeepCopy()
	if nodePool.Status.FailedLaunches >= 3 {
		nodePool.StatusConditions().SetTrueWithReason(v1.ConditionTypeDegraded, "NodeRegistrationFailures",
			"Node registration failing for nodepool, verify cluster networking is configured correctly")
		if err := c.kubeClient.Status().Patch(ctx, nodePool, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	if nodePool.Status.FailedLaunches == 0 {
		nodePool.StatusConditions().SetFalse(v1.ConditionTypeDegraded, "", "")
		if err := c.kubeClient.Status().Patch(ctx, nodePool, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	b := controllerruntime.NewControllerManagedBy(m).
		Named("nodepool.degraded").
		For(&v1.NodePool{}, builder.WithPredicates(nodepoolutils.IsManagedPredicateFuncs(c.cloudProvider))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10})
	for _, nodeClass := range c.cloudProvider.GetSupportedNodeClasses() {
		b.Watches(nodeClass, nodepoolutils.NodeClassEventHandler(c.kubeClient))
	}
	return b.Complete(reconcile.AsReconciler(m.GetClient(), c))
}
