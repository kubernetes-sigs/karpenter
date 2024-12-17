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

package lifecycle

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

type Liveness struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// registrationTTL is a heuristic time that we expect the node to register within
// If we don't see the node within this time, then we should delete the NodeClaim and try again
const registrationTTL = time.Millisecond * 15

// nolint:gocyclo
func (l *Liveness) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	registered := nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered)
	if registered == nil {
		return reconcile.Result{Requeue: true}, nil
	}
	nodePoolName, ok := nodeClaim.Labels[v1.NodePoolLabelKey]
	if !ok {
		return reconcile.Result{}, nil
	}
	nodePool := &v1.NodePool{}
	if err := l.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// if we ever succeed registration, reset failures, old nodeclaim can incorrectly reset this
	if registered.IsTrue() {
		nodePool.Status.FailedLaunches = 0
		if err := l.kubeClient.Status().Update(ctx, nodePool); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		return reconcile.Result{}, nil
	}
	// If the Registered statusCondition hasn't gone True during the TTL since we first updated it, we should terminate the NodeClaim
	// NOTE: ttl has to be stored and checked in the same place since l.clock can advance after the check causing a race
	// If the nodepool is degraded, requeue for the remaining TTL.
	if ttl := registrationTTL - l.clock.Since(registered.LastTransitionTime.Time); ttl > 0 {
		return reconcile.Result{RequeueAfter: ttl}, nil
	}
	// Delete the NodeClaim if we believe the NodeClaim won't register since we haven't seen the node
	// Here we delete the nodeclaim if the node failed to register, we want to retry against the nodeClaim's nodeClass/nodePool 3x.
	// store against a nodepool since nodeclass is not available? nodeclass ref on nodepool, nodepool is 1:1 with nodeclass anyway
	stored := nodePool.DeepCopy()
	nodePool.Status.FailedLaunches += 1
	log.FromContext(ctx).V(1).WithValues("failures", nodePool.Status.FailedLaunches).Info("failed launches so far")
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		if err := l.kubeClient.Status().Patch(ctx, nodePool, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	log.FromContext(ctx).V(1).WithValues("failures", nodePool.Status.FailedLaunches).Info("somehow passing")

	if err := l.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	log.FromContext(ctx).V(1).WithValues("ttl", registrationTTL).Info("terminating due to registration ttl")
	metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
		metrics.ReasonLabel:       "liveness",
		metrics.NodePoolLabel:     nodeClaim.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: nodeClaim.Labels[v1.CapacityTypeLabelKey],
	})

	return reconcile.Result{}, nil
}
