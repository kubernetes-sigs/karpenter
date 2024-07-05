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

package validation

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
}

// NewController is a constructor
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodepool.validation")
	stored := nodePool.DeepCopy()
	err := nodePool.RuntimeValidate()
	if err != nil {
		nodePool.StatusConditions().SetFalse(v1beta1.ConditionTypeValidationSucceeded, "NodePoolValidationFailed", err.Error())
	} else {
		nodePool.StatusConditions().SetTrue(v1beta1.ConditionTypeValidationSucceeded)
	}
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		// We call Update() here rather than Patch() because patching a list with a JSON merge patch
		// can cause races due to the fact that it fully replaces the list on a change
		// https://github.com/kubernetes/kubernetes/issues/111643#issuecomment-2016489732
		if e := c.kubeClient.Status().Update(ctx, nodePool); client.IgnoreNotFound(e) != nil {
			if errors.IsConflict(e) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, e
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodepool.validation").
		For(&v1beta1.NodePool{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
