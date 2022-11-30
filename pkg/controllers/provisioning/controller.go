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

	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/pod"
)

var _ corecontroller.TypedController[*v1.Pod] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient  client.Client
	provisioner *Provisioner
	recorder    events.Recorder
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, provisioner *Provisioner, recorder events.Recorder) corecontroller.Controller {
	return corecontroller.Typed[*v1.Pod](kubeClient, &Controller{
		kubeClient:  kubeClient,
		provisioner: provisioner,
		recorder:    recorder,
	})
}

func (c *Controller) Name() string {
	return "provisioning"
}

// Reconcile the resource
func (c *Controller) Reconcile(_ context.Context, p *v1.Pod) (reconcile.Result, error) {
	if pod.IsProvisionable(p) {
		c.provisioner.Trigger()
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
