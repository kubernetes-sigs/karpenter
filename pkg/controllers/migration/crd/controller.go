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
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/object"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

/*
however many concurrent reconciles
any change to CR re-reconciles, which then lists all CRs

3 Get CR controllers to add annotation -> all resources annotated
1 CRD controller filter on group (karpenter.sh) and cp.GetSupported

*/

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, fmt.Sprintf("migration.crd.%s", req.Name))

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, crd); err != nil {
		return reconcile.Result{}, fmt.Errorf("getting crd %v, %w", req.NamespacedName, err)
	}
	karpenterCrd := lo.Filter(apis.CRDs, func(item *apiextensionsv1.CustomResourceDefinition, _ int) bool {
		return item.ObjectMeta.Name == req.Name
	})
	nodeClassGvk := lo.Map(c.cloudProvider.GetSupportedNodeClasses(), func(nc status.Object, _ int) schema.GroupVersionKind {
		return object.GVK(nc)
	})[0]
	// do nothing for non-Karpenter CRDs
	if len(karpenterCrd) == 0 && !strings.EqualFold(crd.Spec.Names.Kind, nodeClassGvk.Kind) {
		return reconcile.Result{}, nil
	}
	// do nothing for CRDs that have dropped v1beta1
	if !lo.Contains(crd.Status.StoredVersions, "v1beta1") {
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
		return reconcile.Result{}, fmt.Errorf("listing resources for %v, %w", crd.Spec.Names.Kind, err)
	}
	for _, item := range list.Items {
		// requeue if a custom resource is found without annotation
		if !lo.HasKey(item.GetAnnotations(), v1.StoredVersionMigrated) {
			return reconcile.Result{RequeueAfter: 1 * time.Minute}, nil
		}
	}
	// if all custom resources have been updated, patch the CRD
	stored := crd.DeepCopy()
	crd.Status.StoredVersions = []string{"v1"}
	if err := c.kubeClient.Status().Patch(ctx, crd, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		return reconcile.Result{}, fmt.Errorf("patching crd status version %v, %w", req.Name, err)
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("migration.crd").
		For(&apiextensionsv1.CustomResourceDefinition{}).
		Complete(c)
}
