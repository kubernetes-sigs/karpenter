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

	"github.com/awslabs/operatorpkg/object"
	"github.com/samber/lo"

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
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

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1.NodePool) (reconcile.Result, error) {
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
		nodePool.StatusConditions().SetFalse(v1.ConditionTypeNodeClassReady, "NodeClassNotFound", "NodeClass not found on cluster")
	} else if !nodeClass.GetDeletionTimestamp().IsZero() {
		nodePool.StatusConditions().SetFalse(v1.ConditionTypeNodeClassReady, "NodeClassTerminating", "NodeClass is Terminating")
	} else {
		c.setReadyCondition(nodePool, nodeClass)
	}
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
		// can cause races due to the fact that it fully replaces the list on a change
		// Here, we are updating the status condition list
		if err = c.kubeClient.Status().Patch(ctx, nodePool, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}
func (c *Controller) getNodeClass(ctx context.Context, nodePool *v1.NodePool, supportedNC []status.Object) (status.Object, error) {
	nodeClass, ok := lo.Find(supportedNC, func(nc status.Object) bool {
		return object.GVK(nc).Group == nodePool.Spec.Template.Spec.NodeClassRef.Group && object.GVK(nc).Kind == nodePool.Spec.Template.Spec.NodeClassRef.Kind
	})
	if !ok {
		return nodeClass, nil
	}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return nodeClass, nil
}

func (c *Controller) setReadyCondition(nodePool *v1.NodePool, nodeClass status.Object) {
	ready := nodeClass.StatusConditions().Get(status.ConditionReady)
	if ready.IsUnknown() {
		nodePool.StatusConditions().SetFalse(v1.ConditionTypeNodeClassReady, "NodeClassReadinessUnknown", "Node Class Readiness Unknown")
	} else if ready.IsFalse() {
		nodePool.StatusConditions().SetFalse(v1.ConditionTypeNodeClassReady, ready.Reason, ready.Message)
	} else {
		nodePool.StatusConditions().SetTrue(v1.ConditionTypeNodeClassReady)
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
		For(&v1.NodePool{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
