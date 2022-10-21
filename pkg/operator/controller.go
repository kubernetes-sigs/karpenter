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

package operator

import (
	"context"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Controller[T client.Object] interface {
	Reconcile(context.Context, T) (reconcile.Result, error)
	Finalize(context.Context, T) (reconcile.Result, error)
	Register(context.Context, *builder.Builder) *builder.Builder
}

func NewControllerFor[T client.Object](ctx Context, controller Controller[T]) reconcile.Reconciler {
	return &genericcontroller[T]{
		controller: controller,
		client:     ctx.KubeClient,
	}
}

type genericcontroller[T client.Object] struct {
	controller Controller[T]
	client     client.Client
}

func (t *genericcontroller[T]) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	obj := *new(T)

	// Read
	if err := t.client.Get(ctx, req.NamespacedName, obj); err != nil {
		return reconcile.Result{}, err
	}
	// Reconcile
	result, err := t.controller.Reconcile(ctx, obj)
	if err != nil {
		return reconcile.Result{}, err
	}
	// Update
	if err := t.client.Status().Update(ctx, obj); err != nil {
		return reconcile.Result{}, err
	}

	return result, nil
}

func (t *genericcontroller[T]) Register(mgr manager.Manager) error {
	return t.controller.Register(controllerruntime.NewControllerManagedBy(mgr).For(*new(T))).Complete(t)
}
