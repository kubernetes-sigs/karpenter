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

package leasegarbagecollection

import (
	"context"

	v1 "k8s.io/api/coordination/v1"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, l *v1.Lease) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named("lease.garbagecollection").With("lease", client.ObjectKeyFromObject(l)))
	ctx = injection.WithControllerName(ctx, "lease.garbagecollection")

	if l.OwnerReferences != nil || l.Namespace != "kube-node-lease" {
		return reconcile.Result{}, nil
	}
	err := c.kubeClient.Delete(ctx, l)
	if err == nil {
		logging.FromContext(ctx).Debug("found and delete leaked lease")
		NodeLeasesDeletedCounter.WithLabelValues().Inc()
	}

	return reconcile.Result{}, client.IgnoreNotFound(err)
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("lease.garbagecollection").
		For(&v1.Lease{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler[*v1.Lease](m.GetClient(), c))
}
