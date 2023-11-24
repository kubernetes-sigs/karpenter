/*
Copyright 2023 The Kubernetes Authors.

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

	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
)

var _ operatorcontroller.TypedController[*v1.Lease] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
}

// NewController is a constructor
func NewController(kubeClient client.Client) operatorcontroller.Controller {
	return operatorcontroller.Typed[*v1.Lease](kubeClient, &Controller{
		kubeClient: kubeClient,
	})
}

func (c *Controller) Name() string {
	return "lease.garbagecollection"
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, l *v1.Lease) (reconcile.Result, error) {
	if l.OwnerReferences != nil || l.Namespace != "kube-node-lease" {
		return reconcile.Result{}, nil
	}
	err := c.kubeClient.Delete(ctx, l)
	if err == nil {
		logging.FromContext(ctx).Debug("found and delete leaked lease")
		NodeLeaseDeletedCounter.Inc()
	}

	return reconcile.Result{}, client.IgnoreNotFound(err)
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		For(&v1.Lease{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
