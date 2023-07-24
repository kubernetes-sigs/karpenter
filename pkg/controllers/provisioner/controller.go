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

package provisioner

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

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

// Controller is provisioner hash controller that constructs a hash based on the felids that are considered for
// static drift from the provisioner. The hash is placed in the provisioner.objectmeta.Annotation for increased observability.
// The provisioner hash should be found on each machine that is owned that by the provisioner.
type Controller struct {
	kubeClient client.Client
}

func NewController(kubeClient client.Client) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Provisioner](kubeClient, &Controller{
		kubeClient: kubeClient,
	})
}

func (c *Controller) Name() string {
	return "provisioner.hash"
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, p *v1alpha5.Provisioner) (reconcile.Result, error) {
	stored := p.DeepCopy()

	provisionerHash := p.Hash()
	p.ObjectMeta.Annotations = lo.Assign(p.ObjectMeta.Annotations, map[string]string{v1alpha5.ProvisionerHashAnnotationKey: provisionerHash})

	if !equality.Semantic.DeepEqual(stored, p) {
		if err := c.kubeClient.Patch(ctx, p, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}

	return reconcile.Result{}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		For(&v1alpha5.Provisioner{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
