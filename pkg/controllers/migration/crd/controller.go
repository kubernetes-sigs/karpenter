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

package crd

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/object"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

type Controller struct {
	cloudProvider cloudprovider.CloudProvider
	kubeClient    client.Client
}

func NewController(client client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		cloudProvider: cloudProvider,
		kubeClient:    client,
	}
}

func (c *Controller) Reconcile(ctx context.Context, crd *apiextensionsv1.CustomResourceDefinition) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "migration.crd")

	nodeClassGvk := lo.Map(c.cloudProvider.GetSupportedNodeClasses(), func(nc status.Object, _ int) schema.GroupVersionKind {
		return object.GVK(nc)
	})[0]
	if _, ok := lo.Find(apis.CRDs, func(item *apiextensionsv1.CustomResourceDefinition) bool {
		return item.ObjectMeta.Name == crd.Name
	}); !ok && nodeClassGvk.Kind != crd.Spec.Names.Kind {
		return reconcile.Result{}, nil
	}

	// If v1beta1 is not stored, no-op
	if !lo.Contains(crd.Status.StoredVersions, "v1beta1") {
		return reconcile.Result{}, nil
	}
	// We only want to reconcile against CRDs that have v1 as the storage version.
	// Note: we don't need to check this in the resource migration controller since the conversion webhook drops the
	// annotation when applying the v1 resource with the annotation.
	if storageVersion, found := lo.Find(crd.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool {
		return v.Storage
	}); !found || storageVersion.Name != "v1" {
		log.FromContext(ctx).Info("failed to migrate CRD, expected storage version to be v1 (make sure you've upgraded your CRDs)")
		return reconcile.Result{}, nil
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(
		schema.GroupVersionKind{
			Group:   crd.Spec.Group,
			Version: "v1",
			Kind:    crd.Spec.Names.Kind,
		},
	)
	if err := c.kubeClient.List(ctx, list); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing %s(s), %w", crd.Spec.Names.Kind, err)
	}
	for _, item := range list.Items {
		// requeue if a custom resource is found without annotation
		if !lo.HasKey(item.GetAnnotations(), v1.StoredVersionMigratedKey) {
			return reconcile.Result{RequeueAfter: 1 * time.Minute}, nil
		}
	}
	// if all custom resources have been updated, patch the CRD
	stored := crd.DeepCopy()
	crd.Status.StoredVersions = []string{"v1"}
	if err := c.kubeClient.Status().Patch(ctx, crd, client.StrategicMergeFrom(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, fmt.Errorf("patching crd status version %s, %w", crd.Name, err)
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("migration.crd").
		For(&apiextensionsv1.CustomResourceDefinition{}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
