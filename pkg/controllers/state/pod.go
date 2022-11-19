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
	"time"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

var stateRetryPeriod = 1 * time.Minute

const podControllerName = "pod-state"

var _ corecontroller.TypedControllerWithDeletion[*v1.Node] = (*NodeController)(nil)
var _ corecontroller.TypedControllerWithHealthCheck[*v1.Node] = (*NodeController)(nil)

// PodController reconciles pods for the purpose of maintaining state regarding pods that is expensive to compute.
type PodController struct {
	kubeClient client.Client
	cluster    *Cluster
}

func NewPodController(kubeClient client.Client, cluster *Cluster) corecontroller.Controller {
	return corecontroller.For[*v1.Pod](kubeClient, &PodController{
		kubeClient: kubeClient,
		cluster:    cluster,
	})
}

func (c *PodController) Reconcile(ctx context.Context, pod *v1.Pod) (*v1.Pod, reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(podControllerName).With("pod", client.ObjectKeyFromObject(pod)))
	if err := c.cluster.updatePod(ctx, pod); err != nil {
		return nil, reconcile.Result{}, err
	}
	return nil, reconcile.Result{Requeue: true, RequeueAfter: stateRetryPeriod}, nil
}

func (c *PodController) OnDeleted(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
	c.cluster.deletePod(req.NamespacedName)
	return reconcile.Result{}, nil
}

func (c *PodController) Builder(_ context.Context, m manager.Manager) corecontroller.TypedBuilder {
	return corecontroller.NewTypedBuilderAdapter(controllerruntime.
		NewControllerManagedBy(m).
		Named(podControllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}
