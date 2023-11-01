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
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
	np.Annotations = lo.Assign(np.Annotations, nodepoolutil.HashAnnotation(np))

	if !equality.Semantic.DeepEqual(stored, np) {
		if err := nodepoolutil.Patch(ctx, c.kubeClient, stored, np); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
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
