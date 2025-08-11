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
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
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
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster
	recorder      events.Recorder
}

func NewController(kubeClient client.Client, cluster *state.Cluster, recorder events.Recorder, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		cluster:       cluster,
		recorder:      recorder,
	}
}

// Reconcile the resource
func (c *Controller) Reconcile(ctx context.Context, np *v1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "deprovisioning.static")

	if !nodepoolutils.IsManaged(np, c.cloudProvider) {
		return reconcile.Result{}, nil
	}
	if np.Spec.Replicas == nil {
		return reconcile.Result{}, nil
	}

	// TODO: This can be improved to not wait for cluster sync but we need to either rely on a manual
	// update of NodePoolResources so that we track node count accurately, or handle getting NodeClaim count some other way
	if !c.cluster.Synced(ctx) {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}

	nodes := c.cluster.NodePoolResourcesFor(np.Name)[resources.Node]
	desiredReplicas := lo.FromPtr(np.Spec.Replicas)
	currentNodes := nodes.Value()
	nodesToTerminate := currentNodes - desiredReplicas

	// Only handle scale down - scale up is handled by provisioning controller
	if nodesToTerminate <= 0 {
		return reconcile.Result{}, nil
	}

	log.FromContext(ctx).WithValues("NodePool", klog.KObj(np)).
		Info("scaling down static nodepool", "current", currentNodes, "desired", desiredReplicas, "toTerminate", nodesToTerminate)

	// Get all active NodeClaims for this NodePool
	allNodes := c.cluster.Nodes().Active()
	npStateNodes := lo.Filter(allNodes, func(node *state.StateNode, _ int) bool {
		return node.Labels()[v1.NodePoolLabelKey] == np.Name && node.NodeClaim != nil
	})

	// Get deprovisioning candidates
	// rsumukha@ todo : Be more intelligent about picking candidates (SimulateScheduling and get a list)
	candidates := GetDeprovisioningCandidates(ctx, c.kubeClient, npStateNodes, int(nodesToTerminate))

	var scaleDownErrs []error
	actualTerminatedCount := 0
	// Terminate selected NodeClaims
	for _, candidate := range candidates {
		nodeClaim := candidate.NodeClaim
		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return client.IgnoreNotFound(err) != nil }, func() error {
			return c.kubeClient.Delete(ctx, nodeClaim)
		}); err != nil && client.IgnoreNotFound(err) != nil {
			log.FromContext(ctx).Error(err, "failed to delete NodeClaim", "NodeClaim", klog.KObj(nodeClaim))
			scaleDownErrs = append(scaleDownErrs, err)
			continue
		}
		actualTerminatedCount++
		c.recorder.Publish(disruptionevents.Terminating(candidate.Node, candidate.NodeClaim, TerminationReason)...)
		metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
			metrics.ReasonLabel:       pretty.ToSnakeCase(TerminationReason),
			metrics.NodePoolLabel:     candidate.NodeClaim.Labels[v1.NodePoolLabelKey],
			metrics.CapacityTypeLabel: candidate.NodeClaim.Labels[v1.CapacityTypeLabelKey],
		})
	}

	log.FromContext(ctx).WithValues("NodePool", klog.KObj(np)).
		Info("scaled down static nodepool", "current", currentNodes, "desired", desiredReplicas, "terminated", actualTerminatedCount)

	if actualTerminatedCount != int(nodesToTerminate) {
		return reconcile.Result{RequeueAfter: time.Second}, serrors.Wrap(
			errors.NewAggregate(scaleDownErrs),
			"reason", TerminationReason,
		)
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
