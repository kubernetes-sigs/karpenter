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
	"fmt"
	"sync/atomic"

	"github.com/samber/lo"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
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

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	helper "sigs.k8s.io/karpenter/pkg/controllers/static"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster
	clock         clock.Clock
}

func NewController(kubeClient client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, clock clock.Clock) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		cluster:       cluster,
		clock:         clock,
	}
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, np *v1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "deprovisioning.static")

	if !nodepoolutils.IsManaged(np, c.cloudProvider) || np.Spec.Replicas == nil {
		return reconcile.Result{}, nil
	}

	runningNodeClaims, _ := c.cluster.NodePoolState.GetNodeCount(np.Name)
	desiredReplicas := lo.FromPtr(np.Spec.Replicas)
	nodeClaimsToDeprovision := int64(runningNodeClaims) - desiredReplicas

	// Only handle scale down - scale up is handled by provisioning controller
	if nodeClaimsToDeprovision <= 0 {
		return reconcile.Result{}, nil
	}

	log.FromContext(ctx).WithValues("NodePool", klog.KObj(np)).
		Info("scaling down static nodepool", "current", runningNodeClaims, "desired", desiredReplicas, "toDeprovision", nodeClaimsToDeprovision)

	// Get all active NodeClaims for this NodePool
	npStateNodes := helper.GetFilteredNodes(c.cluster, func(node *state.StateNode) bool {
		return node.Labels()[v1.NodePoolLabelKey] == np.Name && node.NodeClaim != nil && !node.MarkedForDeletion()
	})

	// Get deprovisioning candidates
	candidates := GetDeprovisioningCandidates(ctx, c.kubeClient, np, npStateNodes, int(nodeClaimsToDeprovision), c.clock)

	scaleDownErrs := make([]error, len(candidates))
	actualDeprovisionedCount := int64(0)
	// Terminate selected NodeClaims
	workqueue.ParallelizeUntil(ctx, len(candidates), len(candidates), func(i int) {
		candidate := candidates[i]

		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return client.IgnoreNotFound(err) != nil }, func() error {
			return c.kubeClient.Delete(ctx, candidate.NodeClaim)
		}); err != nil && client.IgnoreNotFound(err) != nil {
			log.FromContext(ctx).Error(err, "failed to delete NodeClaim", "NodeClaim", klog.KObj(candidate.NodeClaim))
			scaleDownErrs[i] = err
			return
		}

		atomic.AddInt64(&actualDeprovisionedCount, 1)
		c.cluster.MarkForDeletion(candidate.NodeClaim.Status.ProviderID)
	})

	log.FromContext(ctx).WithValues("NodePool", klog.KObj(np)).
		Info("scaled down static nodepool", "current", runningNodeClaims, "desired", desiredReplicas, "deprovisioned", actualDeprovisionedCount)

	if actualDeprovisionedCount != nodeClaimsToDeprovision {
		return reconcile.Result{}, fmt.Errorf("failed to deprovision %d nodeclaims", nodeClaimsToDeprovision-actualDeprovisionedCount)
	}

	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("deprovisioning.static").
		For(&v1.NodePool{}, builder.WithPredicates(nodepoolutils.IsManagedPredicateFuncs(c.cloudProvider), predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldNP := e.ObjectOld.(*v1.NodePool)
				newNP := e.ObjectNew.(*v1.NodePool)
				return HasNodePoolReplicaCountChanged(oldNP, newNP)
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		})).
		Watches(&v1.NodeClaim{}, nodepoolutils.NodeClaimEventHandler(), builder.WithPredicates(predicate.Funcs{
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

func HasNodePoolReplicaCountChanged(oldNP, newNP *v1.NodePool) bool {
	return lo.FromPtr(oldNP.Spec.Replicas) != lo.FromPtr(newNP.Spec.Replicas)
}
