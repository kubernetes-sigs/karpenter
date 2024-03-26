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

package hash

import (
	"context"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
)

// Controller is hash controller that constructs a hash based on the fields that are considered for static drift.
// The hash is placed in the metadata for increased observability and should be found on each object.
type Controller struct {
	kubeClient client.Client
}

func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, np *v1beta1.NodePool) (reconcile.Result, error) {
	stored := np.DeepCopy()

	if np.Annotations[v1beta1.NodePoolHashVersionAnnotationKey] != v1beta1.NodePoolHashVersion {
		if err := c.updateNodeClaimHash(ctx, np); err != nil {
			return reconcile.Result{}, err
		}
	}
	np.Annotations = lo.Assign(np.Annotations, nodepoolutil.HashAnnotation(np))

	if !equality.Semantic.DeepEqual(stored, np) {
		if err := nodepoolutil.Patch(ctx, c.kubeClient, stored, np); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}

//nolint:revive
type ProvisionerController struct {
	*Controller
}

func NewProvisionerController(kubeClient client.Client) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Provisioner](kubeClient, &ProvisionerController{
		Controller: NewController(kubeClient),
	})
}

func (c *ProvisionerController) Reconcile(ctx context.Context, p *v1alpha5.Provisioner) (reconcile.Result, error) {
	return c.Controller.Reconcile(ctx, nodepoolutil.New(p))
}

func (c *ProvisionerController) Name() string {
	return "provisioner.hash"
}

func (c *ProvisionerController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		For(&v1alpha5.Provisioner{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}

type NodePoolController struct {
	*Controller
}

func NewNodePoolController(kubeClient client.Client) corecontroller.Controller {
	return corecontroller.Typed[*v1beta1.NodePool](kubeClient, &NodePoolController{
		Controller: NewController(kubeClient),
	})
}

func (c *NodePoolController) Name() string {
	return "nodepool.hash"
}

func (c *NodePoolController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		For(&v1beta1.NodePool{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}

// Updating `nodepool-hash-version` annotation inside the controller means a breaking change has made to the hash function calculating
// `nodepool-hash` on both the NodePool and NodeClaim, automatically making the `nodepool-hash` on the NodeClaim different from
// NodePool. Since we can not rely on the hash on the NodeClaims, we will need to re-calculate the hash and update the annotation.
// Look at designs/drift-hash-version.md for more information.
func (c *Controller) updateNodeClaimHash(ctx context.Context, np *v1beta1.NodePool) error {
	if np.IsProvisioner {
		return nil
	}
	ncList := &v1beta1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, ncList, client.MatchingLabels(map[string]string{v1beta1.NodePoolLabelKey: np.Name})); err != nil {
		return err
	}

	errs := make([]error, len(ncList.Items))
	for i := range ncList.Items {
		nc := ncList.Items[i]
		stored := nc.DeepCopy()

		if nc.Annotations[v1beta1.NodePoolHashVersionAnnotationKey] != v1beta1.NodePoolHashVersion {
			nc.Annotations = lo.Assign(nc.Annotations, map[string]string{
				v1beta1.NodePoolHashVersionAnnotationKey: v1beta1.NodePoolHashVersion,
			})

			// Any NodeClaim that is already drifted will remain drifted if the karpenter.sh/nodepool-hash-version doesn't match
			// Since the hashing mechanism has changed we will not be able to determine if the drifted status of the node has changed
			if nc.StatusConditions().GetCondition(v1beta1.Drifted) == nil {
				nc.Annotations = lo.Assign(nc.Annotations, map[string]string{
					v1beta1.NodePoolHashAnnotationKey: np.Hash(),
				})
			}

			if !equality.Semantic.DeepEqual(stored, nc) {
				if err := c.kubeClient.Patch(ctx, &nc, client.MergeFrom(stored)); err != nil {
					errs[i] = client.IgnoreNotFound(err)
				}
			}
		}
	}

	return multierr.Combine(errs...)
}
