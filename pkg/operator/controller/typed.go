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

	"k8s.io/apimachinery/pkg/api/errors"
	"knative.dev/pkg/webhook/resourcesemantics"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Object interface {
	client.Object
	resourcesemantics.GenericCRD
}

type TypedController[T Object] interface {
	Reconcile(context.Context, T) (reconcile.Result, error)
	Builder(context.Context, manager.Manager) *controllerruntime.Builder
	LivenessProbe(*http.Request) error
}

type typedControllerDecorator[T Object] struct {
	typedController TypedController[T]
	kubeClient      client.Client
}

func NewTyped[T Object](kubeClient client.Client, typedController TypedController[T]) Controller {
	return &typedControllerDecorator[T]{
		typedController: typedController,
		kubeClient:      kubeClient,
	}
}

func (t *typedControllerDecorator[T]) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	obj := *new(T)

	// Read
	if err := t.kubeClient.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	mergeFrom := client.MergeFrom(obj.DeepCopyObject().(client.Object))

	// Reconcile
	result, err := t.typedController.Reconcile(ctx, obj)
	if err != nil {
		return reconcile.Result{}, err
	}
	// Patch Status
	if err = t.kubeClient.Status().Patch(ctx, obj, mergeFrom); err != nil {
		return reconcile.Result{}, err
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
