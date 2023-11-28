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

package controller

import (
	"context"
	"reflect"
	"strings"

	"github.com/samber/lo"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
)

type TypedController[T client.Object] interface {
	Reconcile(context.Context, T) (reconcile.Result, error)
	Name() string
	Builder(context.Context, manager.Manager) Builder
}

type FinalizingTypedController[T client.Object] interface {
	TypedController[T]

	Finalize(context.Context, T) (reconcile.Result, error)
}

type typedDecorator[T client.Object] struct {
	kubeClient      client.Client
	typedController TypedController[T]
}

func Typed[T client.Object](kubeClient client.Client, typedController TypedController[T]) Controller {
	return &typedDecorator[T]{
		kubeClient:      kubeClient,
		typedController: typedController,
	}
}

func (t *typedDecorator[T]) Name() string {
	return t.typedController.Name()
}

func (t *typedDecorator[T]) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	obj := reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T) // Create a new pointer to a client.Object
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).
		Named(t.typedController.Name()).
		With(
			strings.ToLower(lo.Must(apiutil.GVKForObject(obj, scheme.Scheme)).Kind),
			lo.Ternary(req.NamespacedName.Namespace != "", req.NamespacedName.String(), req.Name),
		),
	)
	ctx = injection.WithControllerName(ctx, t.typedController.Name())

	if err := t.kubeClient.Get(ctx, req.NamespacedName, obj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	finalizingTypedController, ok := t.typedController.(FinalizingTypedController[T])
	if !obj.GetDeletionTimestamp().IsZero() && ok {
		return finalizingTypedController.Finalize(ctx, obj)
	}
	return t.typedController.Reconcile(ctx, obj)
}

func (t *typedDecorator[T]) Builder(ctx context.Context, m manager.Manager) Builder {
	return t.typedController.Builder(ctx, m)
}
