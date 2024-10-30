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

package health

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

// Controller for the resource
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, clock clock.Clock) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		clock:         clock,
	}
}

func (c *Controller) Reconcile(ctx context.Context, node *corev1.Node) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "node.health")
	nodeHealthCondation := corev1.NodeCondition{}
	cloudProivderPolicy := cloudprovider.RepairPolicy{}

	if !node.GetDeletionTimestamp().IsZero() {
		return reconcile.Result{}, nil
	}

	for _, policy := range c.cloudProvider.RepairPolicy() {
		nodeHealthCondation = nodeutils.GetCondition(node, policy.Type)
		if nodeHealthCondation.Status == policy.Status {
			cloudProivderPolicy = policy
			break
		}
	}

	// From here there are three scenarios to handle:
	// 1. If node is healthy, exit node healhty loop
	if cloudProivderPolicy.Type == "" {
		return reconcile.Result{}, nil
	}

	// 2. If the Node is unhealthy, but has not reached it's full toleration disruption, exit the loop
	dusruptionTime := nodeHealthCondation.LastTransitionTime.Add(cloudProivderPolicy.TolerationDuration)
	if c.clock.Now().Before(dusruptionTime) {
		// Use t.Sub(clock.Now()) instead of time.Until() to ensure we're using the injected clock.
		return reconcile.Result{RequeueAfter: dusruptionTime.Sub(c.clock.Now())}, nil
	}

	nodeClaims, err := nodeutils.GetNodeClaims(ctx, node, c.kubeClient)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodeclaims, %w", err)
	}
	if err := c.annotateTerminationGracePeriodByDefualt(ctx, nodeClaims[0]); err != nil {
		return reconcile.Result{}, fmt.Errorf("annotated termination grace peirod on nodeclaim, %w", err)
	}

	// 3. Otherwise, if the Node is unhealthy we can forcefully remove the node (by deleting it)
	if err := c.kubeClient.Delete(ctx, node); err != nil {
		return reconcile.Result{}, err
	}
	// 4. The deletion timestamp has successfully been set for the Node, update relevant metrics.
	log.FromContext(ctx).V(1).Info("deleting unhealthy node")
	metrics.NodeClaimsDisruptedTotal.With(prometheus.Labels{
		metrics.ReasonLabel:       metrics.UnhealthyReason,
		metrics.NodePoolLabel:     nodeClaims[0].Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: nodeClaims[0].Labels[v1.CapacityTypeLabelKey],
	}).Inc()
	return reconcile.Result{}, nil
}

func (c *Controller) annotateTerminationGracePeriodByDefualt(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	if _, ok := nodeClaim.ObjectMeta.Annotations[v1.NodeClaimTerminationTimestampAnnotationKey]; ok {
		return nil
	}

	stored := nodeClaim.DeepCopy()
	terminationTime := c.clock.Now().Format(time.RFC3339)
	nodeClaim.ObjectMeta.Annotations = lo.Assign(nodeClaim.ObjectMeta.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: terminationTime})

	if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
		return client.IgnoreNotFound(err)
	}

	return nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.health").
		For(&corev1.Node{}).
		WithOptions(
			controller.Options{
				RateLimiter: workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
					workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](100*time.Millisecond, 10*time.Second),
					// 10 qps, 100 bucket size
					&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
				),
				MaxConcurrentReconciles: 100,
			},
		).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
