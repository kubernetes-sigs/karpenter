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

package instancetype

import (
	"context"

	"github.com/awslabs/operatorpkg/reasonable"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
)

type Controller struct {
	provider *Provider
}

func NewController(provider *Provider) *Controller {
	return &Controller{
		provider: provider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool) (reconcile.Result, error) {
	if _, err := c.provider.Get(ctx, nodePool); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: TTL}, nil
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("instancetype").
		For(&v1beta1.NodePool{}).
		WithOptions(controller.Options{RateLimiter: reasonable.RateLimiter()}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
