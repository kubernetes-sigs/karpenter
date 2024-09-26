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

package webhookownerreference

import (
	"context"

	"github.com/awslabs/operatorpkg/object"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/system"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

// Controller for the resource
type Controller[T client.Object] struct {
	kubeClient client.Client
	name       string
}

// NewController is a constructor
func NewController[T client.Object](kubeClient client.Client, name string) *Controller[T] {
	return &Controller[T]{
		kubeClient: kubeClient,
		name:       name,
	}
}

// Reconcile the resource
func (c *Controller[T]) Reconcile(ctx context.Context, obj T) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.name)

	if obj.GetName() != c.name {
		return reconcile.Result{}, nil
	}
	stored := obj.DeepCopyObject().(client.Object)
	obj.SetOwnerReferences(lo.Reject(obj.GetOwnerReferences(), func(o metav1.OwnerReference, _ int) bool {
		return o.APIVersion == "v1" && o.Kind == "Namespace" && o.Name == system.Namespace()
	}))
	if !equality.Semantic.DeepEqual(obj, stored) {
		if err := c.kubeClient.Update(ctx, obj); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller[T]) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.name).
		For(object.New[T]()).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetName() == c.name && lo.ContainsBy(obj.GetOwnerReferences(), func(o metav1.OwnerReference) bool {
				return o.APIVersion == "v1" && o.Kind == "Namespace" && o.Name == system.Namespace()
			})
		})).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
