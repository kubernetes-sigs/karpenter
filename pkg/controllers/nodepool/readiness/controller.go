/*
Copyright The Kubernetes Authors.

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

package readiness

import (
	"context"
	logger "log"

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

// Controller for the resource
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController is a constructor
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodepool.readiness")
	stored := nodePool.DeepCopy()
	supportedNC := c.cloudProvider.GetSupportedNodeClasses()

	if len(supportedNC) == 0 {
		logger.Fatal("no supported node classes found for the cloud provider")
	}
	nodeClass, err := c.getNodeClass(ctx, nodePool, supportedNC)
	if err != nil {
		return reconcile.Result{}, err
	}
	if nodeClass == nil {
		nodePool.StatusConditions().SetFalse(v1beta1.ConditionTypeNodeClassReady, "UnresolvedNodeClass", "Unable to resolve nodeClass")
	} else {
		c.setReadyCondition(nodePool, nodeClass)
	}
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		if err = c.kubeClient.Status().Update(ctx, nodePool); client.IgnoreNotFound(err) != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}
func (c *Controller) getNodeClass(ctx context.Context, nodePool *v1beta1.NodePool, supportedNC []status.Object) (status.Object, error) {
	nodeClass := supportedNC[0]
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return nodeClass, nil
}

func (c *Controller) setReadyCondition(nodePool *v1beta1.NodePool, nodeClass status.Object) {
	ready := nodeClass.StatusConditions().Get(status.ConditionReady)
	if ready.IsUnknown() {
		nodePool.StatusConditions().SetFalse(v1beta1.ConditionTypeNodeClassReady, "NodeClassReadinessUnknown", "Node Class Readiness Unknown")
	} else if ready.IsFalse() {
		nodePool.StatusConditions().SetFalse(v1beta1.ConditionTypeNodeClassReady, ready.Reason, ready.Message)
	} else {
		nodePool.StatusConditions().SetTrue(v1beta1.ConditionTypeNodeClassReady)
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	builder := controllerruntime.NewControllerManagedBy(m)
	for _, nodeClass := range c.cloudProvider.GetSupportedNodeClasses() {
		builder = builder.Watches(
			nodeClass,
			nodepool.NodeClassEventHandler(c.kubeClient),
		)
	}
	return builder.
		Named("nodepool.readiness").
		For(&v1beta1.NodePool{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
