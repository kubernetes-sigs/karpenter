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

package migration

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/awslabs/operatorpkg/object"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

type Object interface {
	client.Object
}

type Controller[T Object] struct {
	kubeClient client.Client
}

func NewController[T Object](client client.Client) *Controller[T] {
	return &Controller[T]{
		kubeClient: client,
	}
}

func (c *Controller[T]) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	o := object.New[T]()

	if err := c.kubeClient.Get(ctx, req.NamespacedName, o); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting object, %w", err)
	}
	stored := o.DeepCopyObject()
	annotations := o.GetAnnotations()
	// if annotation exists on the nodeclaim, do nothing
	if _, ok := annotations[v1.StorageVersion]; ok {
		return reconcile.Result{}, nil
	}
	annotations[v1.StorageVersion] = "v1"

	ctx = injection.WithControllerName(ctx, "migration")
	log.FromContext(ctx).V(1).Info(fmt.Sprintf("annotating v1 stored version on %s: %s",
		o.GetObjectKind().GroupVersionKind().Kind, o.GetName()))
	o.SetAnnotations(annotations)

	if !equality.Semantic.DeepEqual(stored, o) {
		if err := c.kubeClient.Patch(ctx, o, client.MergeFromWithOptions(client.Object(o), client.MergeFromWithOptimisticLock{})); err != nil {
			return reconcile.Result{}, fmt.Errorf("annotating %s: %s",
				o.GetObjectKind().GroupVersionKind().Kind, o.GetName())
		}
	}

	return reconcile.Result{}, nil
}

func (c *Controller[T]) Register(_ context.Context, m manager.Manager) error {
	o := object.New[T]()
	return controllerruntime.NewControllerManagedBy(m).
		For(o).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Named(fmt.Sprintf("migration.%s", strings.ToLower(reflect.TypeOf(object.New[T]()).Elem().Name()))).
		Complete(c)
}
