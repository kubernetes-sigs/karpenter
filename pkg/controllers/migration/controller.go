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
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

type Controller[T client.Object] struct {
	kubeClient client.Client
}

func NewController[T client.Object](client client.Client) *Controller[T] {
	return &Controller[T]{
		kubeClient: client,
	}
}

func (c *Controller[T]) Reconcile(ctx context.Context) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "migration")

	// create object list
	o := object.New[T]()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(object.GVK(o))
	if err := c.kubeClient.List(ctx, list); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing objects, %w", err)
	}

	// update annotations on all CRs
	for _, item := range list.Items {
		stored := item.DeepCopyObject()
		item.SetAnnotations(lo.Assign(item.GetAnnotations(), map[string]string{
			"stored-version": "v1",
		}))
		if !equality.Semantic.DeepEqual(stored, item) {
			if err := c.kubeClient.Update(ctx, &item); err != nil {
				return reconcile.Result{}, fmt.Errorf("annotating: %w", err)
			}
		}
	}

	// To get here, there shouldn't be any resources missing the annotation so filter for and update CRD
	crdList := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := c.kubeClient.List(ctx, crdList); err != nil {
		return reconcile.Result{}, fmt.Errorf("getting crds, %w", err)
	}
	// should filter tighter than this
	crds := lo.Filter(crdList.Items, func(crd apiextensionsv1.CustomResourceDefinition, _ int) bool {
		return strings.Contains(crd.ObjectMeta.Name, strings.ToLower(object.GVK(o).Kind))
	})
	for _, crd := range crds {
		stored := crd.DeepCopy()
		crd.Status.StoredVersions = []string{"v1"}
		if err := c.kubeClient.Status().Patch(ctx, &crd, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			return reconcile.Result{}, fmt.Errorf("updating crd: %w", err)
		}
	}

	return reconcile.Result{}, nil
}

func (c *Controller[T]) Register(_ context.Context, m manager.Manager) error {
	o := object.New[T]()
	return controllerruntime.NewControllerManagedBy(m).
		Named(fmt.Sprintf("migration.%s", strings.ToLower(reflect.TypeOf(o).Elem().Name()))).
		For(o).
		Watches(&apiextensionsv1.CustomResourceDefinition{}, CRDEventHandler(c.kubeClient)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(singleton.AsReconciler(c))
}

func CRDEventHandler(kubeClient client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) (requests []reconcile.Request) {
		crdList := &apiextensionsv1.CustomResourceDefinitionList{}
		if err := kubeClient.List(ctx, crdList); err != nil {
			return requests
		}
		return lo.Map(crdList.Items, func(n apiextensionsv1.CustomResourceDefinition, _ int) reconcile.Request {
			return reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&n),
			}
		})
	})
}
