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

package resource

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/awslabs/operatorpkg/object"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

type Controller[T client.Object] struct {
	kubeClient client.Client
}

/*
This is a generic controller that performs a write to custom resources. This is
needed to update the stored version of the custom resource to v1 from v1beta1.
A no-op write could have been done instead, but the annotation is used so that in case of rollback
to v1beta1, the migration controller on v1beta1 versions will understand
which resources have yet to be migrated.
*/
func NewController[T client.Object](client client.Client) *Controller[T] {
	return &Controller[T]{
		kubeClient: client,
	}
}

func (c *Controller[T]) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	o := object.New[T]()
	ctx = injection.WithControllerName(ctx, fmt.Sprintf("migration.resource.%s", strings.ToLower(object.GVK(o).Kind)))

	if err := c.kubeClient.Get(ctx, req.NamespacedName, o); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting %s, %w", object.GVK(o), err)
	}
	stored := o.DeepCopyObject()
	o.SetAnnotations(lo.Assign(o.GetAnnotations(), map[string]string{
		v1.StoredVersionMigratedKey: "true",
	}))
	// update annotation on CR
	if !equality.Semantic.DeepEqual(stored, o) {
		if err := c.kubeClient.Patch(ctx, o, client.MergeFrom(stored.(client.Object))); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("adding %s annotation to %s, %w", v1.StoredVersionMigratedKey, object.GVK(o), err)
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller[T]) Register(_ context.Context, m manager.Manager) error {
	o := object.New[T]()
	return controllerruntime.NewControllerManagedBy(m).
		Named(fmt.Sprintf("migration.resource.%s", strings.ToLower(reflect.TypeOf(o).Elem().Name()))).
		For(o).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(c)
}
