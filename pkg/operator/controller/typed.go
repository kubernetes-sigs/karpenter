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

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type TypedReconciler[T client.Object] interface {
	Reconcile(context.Context, T) (reconcile.Result, error)
}

type TypedController[T client.Object] interface {
	TypedReconciler[T]

	Finalize(context.Context, T) (reconcile.Result, error)
	Builder(context.Context, manager.Manager) TypedBuilder
	LivenessProbe(*http.Request) error
}

type typedControllerDecorator[T client.Object] struct {
	kubeClient      client.Client
	typedController TypedController[T]
}

func NewTyped[T client.Object](kubeClient client.Client, typedController TypedController[T]) Controller {
	return &typedControllerDecorator[T]{
		kubeClient:      kubeClient,
		typedController: typedController,
	}
}

func (t *typedControllerDecorator[T]) Reconcile(ctx context.Context, req reconcile.Request) (result reconcile.Result, err error) {
	obj := reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T) // Create a new pointer to a client.Object

	// Read
	if err = t.kubeClient.Get(ctx, req.NamespacedName, obj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	stored := obj.DeepCopyObject().(client.Object)
	mergeFrom := client.MergeFrom(stored)

	if !obj.GetDeletionTimestamp().IsZero() {
		// Finalize
		result, err = t.typedController.Finalize(ctx, obj)
		if err != nil {
			return reconcile.Result{}, err
		}
	} else {
		// Reconcile
		result, err = t.typedController.Reconcile(ctx, obj)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Patch Status if changed
	if !equality.Semantic.DeepEqual(obj, stored) {
		if err = t.kubeClient.Status().Patch(ctx, obj, mergeFrom); err != nil {
			return reconcile.Result{}, err
		}
	}
	return result, nil
}

func (t *typedControllerDecorator[T]) Builder(ctx context.Context, mgr manager.Manager) Builder {
	return t.typedController.Builder(ctx, mgr).
		For(*new(T))
}

func (t *typedControllerDecorator[T]) LivenessProbe(req *http.Request) error {
	return t.typedController.LivenessProbe(req)
}
