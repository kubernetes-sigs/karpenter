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

	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type TypedBuilder interface {
	Builder

	// For updates the builder to watch the client.Object passed in
	For(client.Object) TypedBuilder
}

type TypedBuilderControllerRuntimeAdapter struct {
	builder *controllerruntime.Builder
}

func NewTypedBuilderControllerRuntimeAdapter(builder *controllerruntime.Builder) *TypedBuilderControllerRuntimeAdapter {
	return &TypedBuilderControllerRuntimeAdapter{
		builder: builder,
	}
}

func (t *TypedBuilderControllerRuntimeAdapter) For(obj client.Object) TypedBuilder {
	t.builder = t.builder.For(obj)
	return t
}

func (t *TypedBuilderControllerRuntimeAdapter) Complete(r reconcile.Reconciler) error {
	return t.builder.Complete(r)
}

type TypedController[T client.Object] interface {
	Reconcile(context.Context, T) (T, reconcile.Result, error)
	Builder(context.Context, manager.Manager) TypedBuilder
}

type FinalizingTypedController[T client.Object] interface {
	TypedController[T]

	Finalize(context.Context, T) (T, reconcile.Result, error)
}

type typedControllerDecorator[T client.Object] struct {
	kubeClient      client.Client
	typedController TypedController[T]
}

func For[T client.Object](kubeClient client.Client, typedController TypedController[T]) Controller {
	return &typedControllerDecorator[T]{
		kubeClient:      kubeClient,
		typedController: typedController,
	}
}

func (t *typedControllerDecorator[T]) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	obj := reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T) // Create a new pointer to a client.Object

	// Read
	if err := t.kubeClient.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	var updated client.Object
	var result reconcile.Result
	var err error

	finalizingTypedController, ok := t.typedController.(FinalizingTypedController[T])
	if !obj.GetDeletionTimestamp().IsZero() && ok {
		updated, result, err = finalizingTypedController.Finalize(ctx, obj.DeepCopyObject().(T))
	} else {
		// Reconcile
		updated, result, err = t.typedController.Reconcile(ctx, obj.DeepCopyObject().(T))
	}

	if e := t.patch(ctx, obj, updated); e != nil {
		return reconcile.Result{}, multierr.Combine(e, err)
	}
	return result, err
}

func (t *typedControllerDecorator[T]) Builder(ctx context.Context, mgr manager.Manager) Builder {
	return t.typedController.Builder(ctx, mgr).
		For(reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T)) // Create a new pointer to a client.Object
}

func (t *typedControllerDecorator[T]) patch(ctx context.Context, obj, updated client.Object) error {
	// If an updated value returns as nil from the Reconcile function, this means we shouldn't update the object
	if reflect.ValueOf(updated).IsNil() {
		return nil
	}
	// Patch Body if changed
	if !bodyEqual(obj, updated) {
		if err := t.kubeClient.Patch(ctx, updated, client.MergeFrom(obj)); err != nil {
			return err
		}
	}
	// Patch Status if changed
	if !statusEqual(obj, updated) {
		if err := t.kubeClient.Status().Patch(ctx, updated, client.MergeFrom(obj)); err != nil {
			return err
		}
	}
	return nil
}

// bodyEqual compares two objects, ignoring their status and determines if they are deeply-equal
func bodyEqual(a, b client.Object) bool {
	unstructuredA := lo.Must(runtime.DefaultUnstructuredConverter.ToUnstructured(a))
	unstructuredB := lo.Must(runtime.DefaultUnstructuredConverter.ToUnstructured(b))

	// Remove the status fields, so we are only left with non-status info
	delete(unstructuredA, "status")
	delete(unstructuredB, "status")

	return equality.Semantic.DeepEqual(unstructuredA, unstructuredB)
}

// statusEqual compares two object statuses and determines if they are deeply-equal
func statusEqual(a, b client.Object) bool {
	unstructuredA := lo.Must(runtime.DefaultUnstructuredConverter.ToUnstructured(a))
	unstructuredB := lo.Must(runtime.DefaultUnstructuredConverter.ToUnstructured(b))

	return equality.Semantic.DeepEqual(unstructuredA["status"], unstructuredB["status"])
}
