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

package provisioning

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/karpenter-core/pkg/events"
	operatorcontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/pod"

	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const controllerName = "provisioning"

// Controller for the resource
type Controller struct {
	kubeClient  client.Client
	provisioner *Provisioner
	recorder    events.Recorder
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, provisioner *Provisioner, recorder events.Recorder) *Controller {
	return &Controller{
		kubeClient:  kubeClient,
		provisioner: provisioner,
		recorder:    recorder,
	}
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	p := &v1.Pod{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, p); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// Ensure the pod can be provisioned
	if !pod.IsProvisionable(p) {
		return reconcile.Result{}, nil
	}
	c.provisioner.Trigger()
	// TODO: This is only necessary due to a bug in the batcher. Ideally we should retrigger on provisioning error instead
	return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named(controllerName).
		For(&v1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10})
}

func (c *Controller) LivenessProbe(_ *http.Request) error {
	return nil
}
