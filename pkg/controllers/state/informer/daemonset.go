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

package informer

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/controllers/state"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

// NewController constructs a controller instance
func NewDaemonSetController(kubeClient client.Client, cluster *state.Cluster) corecontroller.Controller {
	return &Controller{
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

func (c *Controller) Name() string {
	return "daemonset"
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	daemonSet := appsv1.DaemonSet{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, &daemonSet); err != nil {
		if errors.IsNotFound(err) {
			// notify cluster state of the daemonset deletion
			c.cluster.DeleteDaemonSet(req.NamespacedName)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	return reconcile.Result{RequeueAfter: time.Minute}, c.cluster.UpdateDaemonSet(ctx, &daemonSet)
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&appsv1.DaemonSet{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
