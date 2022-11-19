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

package state

import (
	"context"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

const provisionerControllerName = "provisioner-state"

var _ corecontroller.TypedController[*v1alpha5.Provisioner] = (*ProvisionerController)(nil)

// ProvisionerController reconciles provisioners to re-trigger consolidation on change.
type ProvisionerController struct {
	kubeClient client.Client
	cluster    *Cluster
}

func NewProvisionerController(kubeClient client.Client, cluster *Cluster) corecontroller.Controller {
	return corecontroller.For[*v1alpha5.Provisioner](kubeClient, &ProvisionerController{
		kubeClient: kubeClient,
		cluster:    cluster,
	})
}

func (c *ProvisionerController) Reconcile(_ context.Context, _ *v1alpha5.Provisioner) (*v1alpha5.Provisioner, reconcile.Result, error) {
	// Something changed in the provisioner so we should re-consider consolidation
	c.cluster.recordConsolidationChange()
	return nil, reconcile.Result{}, nil
}

func (c *ProvisionerController) Builder(_ context.Context, m manager.Manager) corecontroller.TypedBuilder {
	return corecontroller.NewTypedBuilderControllerRuntimeAdapter(controllerruntime.
		NewControllerManagedBy(m).
		Named(provisionerControllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithEventFilter(predicate.Funcs{DeleteFunc: func(event event.DeleteEvent) bool { return false }}))
}
