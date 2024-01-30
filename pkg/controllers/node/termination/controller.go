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

package termination

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

var _ operatorcontroller.FinalizingTypedController[*v1.Node] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	terminator    *terminator.Terminator
	recorder      events.Recorder
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, terminator *terminator.Terminator, recorder events.Recorder) operatorcontroller.Controller {
	return operatorcontroller.Typed[*v1.Node](kubeClient, &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		terminator:    terminator,
		recorder:      recorder,
	})
}

func (c *Controller) Name() string {
	return "node.termination"
}

func (c *Controller) Reconcile(_ context.Context, _ *v1.Node) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

//nolint:gocyclo
func (c *Controller) Finalize(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(node, v1beta1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	if err := c.deleteAllNodeClaims(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("deleting nodeclaims, %w", err)
	}

	nodeGracePeriodExpirationTime := c.terminationGracePeriodExpirationTime(ctx, node)
	if nodeGracePeriodExpirationTime != nil && time.Now().After(*nodeGracePeriodExpirationTime) {
		taint := v1beta1.DisruptionNonGracefulShutdown
		logging.FromContext(ctx).With("taint.Key", taint.Key).With("taint.Effect", taint.Effect).With("taint.Value", taint.Value).Infof("node's terminationGracePeriod has expired, tainting")
		if err := c.terminator.Taint(ctx, node, taint); err != nil {
			return reconcile.Result{}, fmt.Errorf("tainting node: %w", err)
		}
	}

	if err := c.terminator.Taint(ctx, node, v1beta1.DisruptionNoScheduleTaint); err != nil {
		return reconcile.Result{}, fmt.Errorf("tainting node with karpenter.sh/disruption taint, %w", err)
	}
	if err := c.terminator.Drain(ctx, node, nodeGracePeriodExpirationTime); err != nil {
		if !terminator.IsNodeDrainError(err) {
			return reconcile.Result{}, fmt.Errorf("draining node, %w", err)
		}
		c.recorder.Publish(terminatorevents.NodeFailedToDrain(node, err))
		// If the underlying NodeClaim no longer exists, we want to delete to avoid trying to gracefully draining
		// on nodes that are no longer alive. We do a check on the Ready condition of the node since, even
		// though the CloudProvider says the instance is not around, we know that the kubelet process is still running
		// if the Node Ready condition is true
		// Similar logic to: https://github.com/kubernetes/kubernetes/blob/3a75a8c8d9e6a1ebd98d8572132e675d4980f184/staging/src/k8s.io/cloud-provider/controllers/nodelifecycle/node_lifecycle_controller.go#L144
		if nodeutils.GetCondition(node, v1.NodeReady).Status != v1.ConditionTrue {
			if _, err := c.cloudProvider.Get(ctx, node.Spec.ProviderID); err != nil {
				if cloudprovider.IsNodeClaimNotFoundError(err) {
					return reconcile.Result{}, c.removeFinalizer(ctx, node)
				}
				return reconcile.Result{}, fmt.Errorf("getting nodeclaim, %w", err)
			}
		}

		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}
	if err := c.removeFinalizer(ctx, node); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (c *Controller) deleteAllNodeClaims(ctx context.Context, node *v1.Node) error {
	nodeClaimList := &v1beta1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaimList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
		return err
	}
	for i := range nodeClaimList.Items {
		// If we still get the NodeClaim, but it's already marked as terminating, we don't need to call Delete again
		if nodeClaimList.Items[i].DeletionTimestamp.IsZero() {
			if err := c.kubeClient.Delete(ctx, &nodeClaimList.Items[i]); err != nil {
				return client.IgnoreNotFound(err)
			}
		}
	}
	return nil
}

func (c *Controller) removeFinalizer(ctx context.Context, n *v1.Node) error {
	stored := n.DeepCopy()
	controllerutil.RemoveFinalizer(n, v1beta1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, n) {
		if err := c.kubeClient.Patch(ctx, n, client.StrategicMergeFrom(stored)); err != nil {
			return client.IgnoreNotFound(fmt.Errorf("patching node, %w", err))
		}
		metrics.NodesTerminatedCounter.With(prometheus.Labels{
			metrics.NodePoolLabel: n.Labels[v1beta1.NodePoolLabelKey],
		}).Inc()
		// We use stored.DeletionTimestamp since the api-server may give back a node after the patch without a deletionTimestamp
		TerminationSummary.With(prometheus.Labels{
			metrics.NodePoolLabel: n.Labels[v1beta1.NodePoolLabelKey],
		}).Observe(time.Since(stored.DeletionTimestamp.Time).Seconds())
		logging.FromContext(ctx).Infof("deleted node")
	}
	return nil
}

func (c *Controller) terminationGracePeriodExpirationTime(ctx context.Context, node *v1.Node) *time.Time {

	nodeClaim := &v1beta1.NodeClaim{}

	nodeClaimRef, ok := lo.Find(node.OwnerReferences, func(o metav1.OwnerReference) bool {
		// verify that we're finding Karpenter's NodeClaim of any version permutation
		groupVersion, err := schema.ParseGroupVersion(o.APIVersion)
		if err != nil {
			logging.FromContext(ctx).Errorf("could not parse Node ownerRef: %v", err)
			return false
		}
		return strings.HasPrefix(groupVersion.Group, v1beta1.Group) && o.Kind == "NodeClaim"
	})

	if !ok {
		logging.FromContext(ctx).Errorf("node has no owner, could not find NodeClaim")
		return nil
	}

	nodeClaimName := types.NamespacedName{
		Name: nodeClaimRef.Name,
	}

	if err := c.kubeClient.Get(ctx, nodeClaimName, nodeClaim); err != nil {
		logging.FromContext(ctx).Errorf("node has an owner, but could not find NodeClaim via the k8s api")
		return nil
	}

	if nodeClaim.Spec.TerminationGracePeriod != nil && nodeClaim.DeletionTimestamp != nil {
		expirationTime := nodeClaim.DeletionTimestamp.Time.Add(nodeClaim.Spec.TerminationGracePeriod.Duration)
		c.recorder.Publish(terminatorevents.NodeTerminationGracePeriod(node, expirationTime, nodeClaim.Spec.TerminationGracePeriod.String()))
		return &expirationTime
	}

	return nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1.Node{}).
		WithOptions(
			controller.Options{
				RateLimiter: workqueue.NewMaxOfRateLimiter(
					workqueue.NewItemExponentialFailureRateLimiter(100*time.Millisecond, 10*time.Second),
					// 10 qps, 100 bucket size
					&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
				),
				MaxConcurrentReconciles: 100,
			},
		))
}
