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
	"net/http"
	"reflect"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type TypedReconciler[T client.Object] interface {
	Reconcile(context.Context, T) (T, reconcile.Result, error)
}

type TypedController[T client.Object] interface {
	TypedReconciler[T]

	Builder(context.Context, manager.Manager) TypedBuilder
	LivenessProbe(*http.Request) error
}

type NotFoundHandler[T client.Object] interface {
	OnNotFound(context.Context, reconcile.Request) (reconcile.Result, error)
}

type FinalizerHandler[T client.Object] interface {
	OnFinalize(context.Context, T) (T, reconcile.Result, error)
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

//nolint:gocyclo
func (t *typedControllerDecorator[T]) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	obj := reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T) // Create a new pointer to a client.Object

	// Read
	if err := t.kubeClient.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			if notFoundHandler, ok := t.typedController.(NotFoundHandler[T]); ok {
				return notFoundHandler.OnNotFound(ctx, req)
			}
		}
		return reconcile.Result{}, err
	}
	var updated T
	var result reconcile.Result
	var err error

	finalizingHandler, ok := t.typedController.(FinalizerHandler[T])
	if !obj.GetDeletionTimestamp().IsZero() && ok {
		// Finalize if the controller implements the finalizing interface
		updated, result, err = finalizingHandler.OnFinalize(ctx, obj.DeepCopyObject().(T))
		if err != nil {
			return reconcile.Result{}, err
		}
	} else {
		// Reconcile
		updated, result, err = t.typedController.Reconcile(ctx, obj.DeepCopyObject().(T))
		if err != nil {
			return reconcile.Result{}, err
		}
	}
	// If an updated value returns as nil from the Reconcile function, this means we shouldn't update the object
	if reflect.ValueOf(updated).IsNil() {
		return result, nil
	}
	// Patch Body if changed
	if !bodyEqual(obj, updated) {
		if err = t.kubeClient.Patch(ctx, updated, client.MergeFrom(obj)); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Patch Status if changed
	if !statusEqual(obj, updated) {
		if err = t.kubeClient.Status().Patch(ctx, updated, client.MergeFrom(obj)); err != nil {
			return reconcile.Result{}, err
		}
	}
	return result, nil
}

func (t *typedControllerDecorator[T]) Builder(ctx context.Context, mgr manager.Manager) Builder {
	return t.typedController.Builder(ctx, mgr).
		For(reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T)) // Create a new pointer to a client.Object
}

func (t *typedControllerDecorator[T]) LivenessProbe(req *http.Request) error {
	return t.typedController.LivenessProbe(req)
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
