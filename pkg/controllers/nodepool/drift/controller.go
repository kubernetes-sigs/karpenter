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

package drift

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodepool.drift")
	stored := nodePool.DeepCopy()

	if !nodepoolutils.IsManaged(nodePool, c.cloudProvider) {
		return reconcile.Result{}, nil
	}

	nodeClaims, err := nodeclaimutils.ListManaged(ctx, c.kubeClient, c.cloudProvider, nodeclaimutils.ForNodePool(nodePool.Name))
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodeclaims: %w", err)
	}

	driftedNodeClaimCount := lo.CountBy(nodeClaims, func(nodeClaim *v1.NodeClaim) bool {
		driftedCondition := nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)
		return driftedCondition != nil && driftedCondition.IsTrue()
	})

	// Update the Drifted condition on the NodePool
	hasDriftedNodeClaims := driftedNodeClaimCount > 0
	if hasDriftedNodeClaims {
		nodePool.StatusConditions().SetTrueWithReason(
			v1.ConditionTypeNodeClaimsDrifted,
			"NodeClaimsDriftedExist",
			fmt.Sprintf("%d NodeClaim(s) managed by this NodePool have drifted", driftedNodeClaimCount),
		)
		// All the other cases, No managed NodeClaims or all NodeClaim with Drifted condition as false, unknown even nil
	} else {
		nodePool.StatusConditions().SetFalse(
			v1.ConditionTypeNodeClaimsDrifted,
			"ZeroNodeClaimsDrifted",
			"All NodeClaims are in sync with the NodePool configuration",
		)
	}

	nodePool.Status.DriftedNodeClaimCount = driftedNodeClaimCount
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

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	// Create a NodeClaim predicate that only triggers reconciliation when the Drifted condition changes
	driftedStatusChangePredicate := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNodeClaim, ok := e.ObjectOld.(*v1.NodeClaim)
			if !ok {
				return false
			}
			newNodeClaim, ok := e.ObjectNew.(*v1.NodeClaim)
			if !ok {
				return false
			}

			oldDrifted := oldNodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)
			newDrifted := newNodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)

			if (oldDrifted == nil && newDrifted == nil) ||
				(oldDrifted != nil && newDrifted != nil && oldDrifted.Status == newDrifted.Status) {
				return false
			}

			return true
		},
		CreateFunc: func(e event.CreateEvent) bool {
			nodeClaim, ok := e.Object.(*v1.NodeClaim)
			if !ok {
				return false
			}
			// Only reconcile if the nodeclaim is created with a drifted condition
			return nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted) != nil
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Always reconcile on deletion to recalculate drift nodeclaims count
			return true
		},
	}

	return controllerruntime.NewControllerManagedBy(m).
		Named("nodepool.drift").
		For(&v1.NodePool{}, builder.WithPredicates(nodepoolutils.IsManagedPredicateFuncs(c.cloudProvider))).
		Watches(
			&v1.NodeClaim{},
			nodepoolutils.NodeClaimEventHandler(),
			builder.WithPredicates(driftedStatusChangePredicate),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
