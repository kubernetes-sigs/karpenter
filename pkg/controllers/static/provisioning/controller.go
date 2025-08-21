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

package static

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samber/lo"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	provisioner   *provisioning.Provisioner
	cluster       *state.Cluster
	recorder      events.Recorder
}

func NewController(kubeClient client.Client, cluster *state.Cluster, recorder events.Recorder, cloudProvider cloudprovider.CloudProvider, provisioner *provisioning.Provisioner, clock clock.Clock) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		cluster:       cluster,
		recorder:      recorder,
		provisioner:   provisioning.NewProvisioner(kubeClient, recorder, cloudProvider, cluster, clock),
	}
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, np *v1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "provisioning.static")

	if !nodepoolutils.IsManaged(np, c.cloudProvider) || !np.StatusConditions().Root().IsTrue() || np.Spec.Replicas == nil {
		return reconcile.Result{}, nil
	}

	nodes := TotalNodesForNodePool(c.cluster, np)
	// Size down of replicas will be handled in disruption controller to drain nodes and delete NodeClaims
	if nodes >= lo.FromPtr(np.Spec.Replicas) {
		return reconcile.Result{}, nil
	}

	countNodeClaimsToProvision := ComputeNodeClaimsToProvision(c.cluster, np, nodes)
	if countNodeClaimsToProvision <= 0 {
		log.FromContext(ctx).WithValues("NodePool", klog.KObj(np)).Info("nodepool node limit reached")
		return reconcile.Result{RequeueAfter: time.Second * 30}, nil
	}

	its, err := c.cloudProvider.GetInstanceTypes(ctx, np)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return reconcile.Result{}, fmt.Errorf("timed out while getting instance types: %w", err)
		}
		log.FromContext(ctx).WithValues("NodePool", klog.KObj(np)).Error(err, "failed to resolve instance types")
		return reconcile.Result{}, fmt.Errorf("failed to resolve instance types: %w", err)
	}

	nodeClaims := GetStaticNodeClaimsToProvision(np, its, countNodeClaimsToProvision)

	_, err = c.provisioner.CreateNodeClaims(ctx, nodeClaims, provisioning.WithReason(metrics.ProvisionedReason))
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("creating nodeclaims, %w", err)
	}

	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("provisioning.static").
		For(&v1.NodePool{}, builder.WithPredicates(nodepoolutils.IsManagedPredicateFuncs(c.cloudProvider), predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldNP := e.ObjectOld.(*v1.NodePool)
				newNP := e.ObjectNew.(*v1.NodePool)
				return HasNodePoolReplicaOrStatusChanged(oldNP, newNP)
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		})).
		Watches(&v1.NodeClaim{}, nodeutils.NodeClaimEventHandler(c.kubeClient), builder.WithPredicates(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return e.ObjectOld.GetDeletionTimestamp().IsZero() && !e.ObjectNew.GetDeletionTimestamp().IsZero()
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return true
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func HasNodePoolReplicaOrStatusChanged(oldNP, newNP *v1.NodePool) bool {
	return lo.FromPtr(oldNP.Spec.Replicas) != lo.FromPtr(newNP.Spec.Replicas) || (!oldNP.StatusConditions().Root().IsTrue() && newNP.StatusConditions().Root().IsTrue())
}
