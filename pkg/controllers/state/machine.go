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

	"k8s.io/apimachinery/pkg/api/errors"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha1"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

// MachineController reconciles inflightNodes for the purpose of maintaining state regarding nodes that is expensive to compute.
type MachineController struct {
	kubeClient client.Client
	cluster    *Cluster
}

// NewMachineController constructs a controller instance
func NewMachineController(kubeClient client.Client, cluster *Cluster) corecontroller.Controller {
	return &MachineController{
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

func (c *MachineController) Name() string {
	return "machine-state"
}

func (c *MachineController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(c.Name()).With("machine", req.NamespacedName.Name))
	machine := &v1alpha1.Machine{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, machine); err != nil {
		if errors.IsNotFound(err) {
			c.cluster.DeleteMachine(req.Name)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if machine.Status.ProviderID == "" {
		return reconcile.Result{}, nil
	}
	c.cluster.UpdateMachine(machine)
	return reconcile.Result{RequeueAfter: stateRetryPeriod}, nil
}

func (c *MachineController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha1.Machine{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}
