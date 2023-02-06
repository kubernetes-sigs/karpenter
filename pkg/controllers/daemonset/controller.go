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

package daemonset

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/controllers/state"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

var _ corecontroller.TypedController[*appsv1.DaemonSet] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cluster *state.Cluster) corecontroller.Controller {
	return corecontroller.Typed[*appsv1.DaemonSet](kubeClient, &Controller{
		kubeClient: kubeClient,
		cluster:    cluster,
	})
}

func (c *Controller) Name() string {
	return "daemonset"
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, daemonSet *appsv1.DaemonSet) (reconcile.Result, error) {
	pods := &v1.PodList{}
	err := c.kubeClient.List(ctx, pods)
	if err != nil {
		return reconcile.Result{}, err
	}

	for index := range pods.Items {
		if containsOwnerRef(&pods.Items[index], daemonSet.UID) {
			c.cluster.UpdateDaemonSetCache(daemonSet, &pods.Items[index])
		}
	}

	return reconcile.Result{RequeueAfter: time.Minute}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&appsv1.DaemonSet{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}

func containsOwnerRef(pod *v1.Pod, matchUID types.UID) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.UID == matchUID {
			return true
		}
	}
	return false
}
