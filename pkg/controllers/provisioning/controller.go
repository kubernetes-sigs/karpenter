/*
Copyright 2023 The Kubernetes Authors.

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
	"time"

	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/events"
	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

var _ operatorcontroller.TypedController[*v1.Pod] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient  client.Client
	provisioner *Provisioner
	recorder    events.Recorder
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, provisioner *Provisioner, recorder events.Recorder) operatorcontroller.Controller {
	return operatorcontroller.Typed[*v1.Pod](kubeClient, &Controller{
		kubeClient:  kubeClient,
		provisioner: provisioner,
		recorder:    recorder,
	})
}

func (c *Controller) Name() string {
	return "provisioner.trigger"
}

// Reconcile the resource
func (c *Controller) Reconcile(_ context.Context, p *v1.Pod) (reconcile.Result, error) {
	if !pod.IsProvisionable(p) {
		return reconcile.Result{}, nil
	}
	c.provisioner.Trigger()
	// Continue to requeue until the pod is no longer provisionable. Pods may
	// not be scheduled as expected if new pods are created while nodes are
	// coming online. Even if a provisioning loop is successful, the pod may
	// require another provisioning loop to become schedulable.
	return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
