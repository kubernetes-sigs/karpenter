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

package termination

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

// Controller is a NodeClaim Termination controller that triggers deletion of the Node and the
// CloudProvider NodeClaim through its graceful termination mechanism
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController is a constructor for the NodeClaim Controller
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (*Controller) Name() string {
	return "machine.termination"
}

func (c *Controller) Reconcile(_ context.Context, _ *v1beta1.NodeClaim) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (c *Controller) Finalize(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", nodeClaim.Status.NodeName, "provider-id", nodeClaim.Status.ProviderID))
	stored := nodeClaim.DeepCopy()
	if !controllerutil.ContainsFinalizer(nodeClaim, v1beta1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	nodes, err := nodeclaimutil.AllNodesForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		return reconcile.Result{}, err
	}
	for _, node := range nodes {
		// We delete nodes to trigger the node finalization and deletion flow
		if err = c.kubeClient.Delete(ctx, node); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, err
		}
	}
	// We wait until all the nodes associated with this nodeClaim have completed their deletion before triggering the finalization of the nodeClaim
	if len(nodes) > 0 {
		return reconcile.Result{}, nil
	}
	if nodeClaim.Status.ProviderID != "" || nodeClaim.Annotations[v1alpha5.MachineLinkedAnnotationKey] != "" {
		if err = c.cloudProvider.Delete(ctx, nodeClaim); cloudprovider.IgnoreNodeClaimNotFoundError(err) != nil {
			return reconcile.Result{}, fmt.Errorf("terminating cloudprovider instance, %w", err)
		}
	}
	controllerutil.RemoveFinalizer(nodeClaim, v1beta1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		if err = nodeclaimutil.Patch(ctx, c.kubeClient, stored, nodeClaim); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(fmt.Errorf("removing termination finalizer, %w", err))
		}
		logging.FromContext(ctx).Infof("deleted %s", lo.Ternary(nodeClaim.IsMachine, "machine", "nodeclaim"))
	}
	return reconcile.Result{}, nil
}

var _ corecontroller.FinalizingTypedController[*v1beta1.NodeClaim] = (*NodeClaimController)(nil)

type NodeClaimController struct {
	*Controller
}

func NewNodeClaimController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1beta1.NodeClaim](kubeClient, &NodeClaimController{
		Controller: NewController(kubeClient, cloudProvider),
	})
}

func (*NodeClaimController) Name() string {
	return "nodeclaim.termination"
}

func (c *NodeClaimController) Reconcile(_ context.Context, _ *v1beta1.NodeClaim) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (c *NodeClaimController) Finalize(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("nodepool", nodeClaim.Labels[v1beta1.NodePoolLabelKey]))
	return c.Controller.Finalize(ctx, nodeClaim)
}

func (c *NodeClaimController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodeClaim{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Watches(
			&v1.Node{},
			nodeclaimutil.NodeEventHandler(c.kubeClient),
			// Watch for node deletion events
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool { return false },
				UpdateFunc: func(e event.UpdateEvent) bool { return false },
				DeleteFunc: func(e event.DeleteEvent) bool { return true },
			}),
		).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewMaxOfRateLimiter(
				workqueue.NewItemExponentialFailureRateLimiter(time.Second, time.Minute),
				// 10 qps, 100 bucket size
				&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			MaxConcurrentReconciles: 100, // higher concurrency limit since we want fast reaction to termination
		}))
}

var _ corecontroller.FinalizingTypedController[*v1alpha5.Machine] = (*MachineController)(nil)

type MachineController struct {
	*Controller
}

func NewMachineController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &MachineController{
		Controller: NewController(kubeClient, cloudProvider),
	})
}

func (*MachineController) Name() string {
	return "machine.termination"
}

func (c *MachineController) Reconcile(_ context.Context, _ *v1alpha5.Machine) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (c *MachineController) Finalize(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provisioner", machine.Labels[v1alpha5.ProvisionerNameLabelKey]))
	return c.Controller.Finalize(ctx, nodeclaimutil.New(machine))
}

func (c *MachineController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Machine{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Watches(
			&v1.Node{},
			machineutil.NodeEventHandler(c.kubeClient),
			// Watch for node deletion events
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool { return false },
				UpdateFunc: func(e event.UpdateEvent) bool { return false },
				DeleteFunc: func(e event.DeleteEvent) bool { return true },
			}),
		).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewMaxOfRateLimiter(
				workqueue.NewItemExponentialFailureRateLimiter(time.Second, time.Minute),
				// 10 qps, 100 bucket size
				&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			MaxConcurrentReconciles: 100, // higher concurrency limit since we want fast reaction to termination
		}))
}
