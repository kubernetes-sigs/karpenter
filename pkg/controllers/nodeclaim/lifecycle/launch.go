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
	"fmt"

	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type Launch struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cache         *cache.Cache // exists due to eventual consistency on the cache
	recorder      events.Recorder
}

func (l *Launch) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	if nodeClaim.StatusConditions().GetCondition(v1beta1.Launched).IsTrue() {
		return reconcile.Result{}, nil
	}

	var err error
	var created *v1beta1.NodeClaim

	// One of the following scenarios can happen with a NodeClaim that isn't marked as launched:
	//  1. It was already launched by the CloudProvider but the client-go cache wasn't updated quickly enough or
	//     patching failed on the status. In this case, we use the in-memory cached value for the created NodeClaim.
	//  2. It is a standard NodeClaim launch where we should call CloudProvider Create() and fill in details of the launched
	//     NodeClaim into the NodeClaim CR.
	if ret, ok := l.cache.Get(string(nodeClaim.UID)); ok {
		created = ret.(*v1beta1.NodeClaim)
	} else {
		created, err = l.launchNodeClaim(ctx, nodeClaim)
	}
	// Either the Node launch failed or the Node was deleted due to InsufficientCapacity/NotFound
	if err != nil || created == nil {
		if cloudprovider.IsNodeClassNotReadyError(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	l.cache.SetDefault(string(nodeClaim.UID), created)
	nodeClaim = PopulateNodeClaimDetails(nodeClaim, created)
	nodeClaim.StatusConditions().MarkTrue(v1beta1.Launched)
	metrics.NodeClaimsLaunchedCounter.With(prometheus.Labels{
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	}).Inc()

	return reconcile.Result{}, nil
}

func (l *Launch) launchNodeClaim(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (*v1beta1.NodeClaim, error) {
	created, err := l.cloudProvider.Create(ctx, nodeClaim)
	if err != nil {
		switch {
		case cloudprovider.IsInsufficientCapacityError(err):
			l.recorder.Publish(InsufficientCapacityErrorEvent(nodeClaim, err))
			logging.FromContext(ctx).Error(err)
			if err = l.kubeClient.Delete(ctx, nodeClaim); err != nil {
				return nil, client.IgnoreNotFound(err)
			}
			metrics.NodeClaimsTerminatedCounter.With(prometheus.Labels{
				metrics.ReasonLabel:   "insufficient_capacity",
				metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
			}).Inc()
			return nil, nil
		case cloudprovider.IsNodeClassNotReadyError(err):
			l.recorder.Publish(NodeClassNotReadyEvent(nodeClaim, err))
			nodeClaim.StatusConditions().MarkFalse(v1beta1.Launched, "LaunchFailed", truncateMessage(err.Error()))
			return nil, fmt.Errorf("launching nodeclaim, %w", err)
		default:
			nodeClaim.StatusConditions().MarkFalse(v1beta1.Launched, "LaunchFailed", truncateMessage(err.Error()))
			return nil, fmt.Errorf("launching nodeclaim, %w", err)
		}
	}
	logging.FromContext(ctx).With(
		"provider-id", created.Status.ProviderID,
		"instance-type", created.Labels[v1.LabelInstanceTypeStable],
		"zone", created.Labels[v1.LabelTopologyZone],
		"capacity-type", created.Labels[v1beta1.CapacityTypeLabelKey],
		"allocatable", created.Status.Allocatable).Infof("launched nodeclaim")
	return created, nil
}

func PopulateNodeClaimDetails(nodeClaim, retrieved *v1beta1.NodeClaim) *v1beta1.NodeClaim {
	// These are ordered in priority order so that user-defined nodeClaim labels and requirements trump retrieved labels
	// or the static nodeClaim labels
	nodeClaim.Labels = lo.Assign(
		retrieved.Labels, // CloudProvider-resolved labels
		scheduling.NewNodeSelectorRequirements(nodeClaim.Spec.Requirements...).Labels(), // Single-value requirement resolved labels
		nodeClaim.Labels, // User-defined labels
	)
	nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, retrieved.Annotations)
	nodeClaim.Status.ProviderID = retrieved.Status.ProviderID
	nodeClaim.Status.ImageID = retrieved.Status.ImageID
	nodeClaim.Status.Allocatable = retrieved.Status.Allocatable
	nodeClaim.Status.Capacity = retrieved.Status.Capacity
	return nodeClaim
}

func truncateMessage(msg string) string {
	if len(msg) < 300 {
		return msg
	}
	return msg[:300] + "..."
}
