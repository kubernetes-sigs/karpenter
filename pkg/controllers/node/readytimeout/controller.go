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

package readytimeout

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

var allowedUnhealthyPercent = intstr.FromString("20%")

// Controller for the resource
type Controller struct {
	clock         clock.Clock
	recorder      events.Recorder
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, clock clock.Clock, recorder events.Recorder) *Controller {
	return &Controller{
		clock:         clock,
		recorder:      recorder,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.readytimeout").
		For(&corev1.Node{}, builder.WithPredicates(nodeutils.IsManagedPredicateFuncs(c.cloudProvider))).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func (c *Controller) Reconcile(ctx context.Context, node *corev1.Node) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "node.readytimeout")

	// Check if feature gate is enabled
	opts := options.FromContext(ctx)
	if !opts.FeatureGates.NodeReadyTimeoutRecovery {
		return reconcile.Result{}, nil
	}

	// Validate that the node is owned by us
	nodeClaim, err := nodeutils.NodeClaimForNode(ctx, c.kubeClient, node)
	if err != nil {
		return reconcile.Result{}, nodeutils.IgnoreDuplicateNodeClaimError(nodeutils.IgnoreNodeClaimNotFoundError(err))
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("NodeClaim", klog.KObj(nodeClaim)))

	// Check if the node is ready
	readyCondition := nodeutils.GetCondition(node, corev1.NodeReady)
	if readyCondition.Status == corev1.ConditionTrue {
		// Node is ready, nothing to do
		return reconcile.Result{}, nil
	}

	// Check if we have exceeded the timeout
	nodeAge := c.clock.Since(node.CreationTimestamp.Time)
	if nodeAge < opts.NodeReadyTimeout {
		// Haven't reached timeout yet, requeue at timeout
		return reconcile.Result{RequeueAfter: opts.NodeReadyTimeout - nodeAge}, nil
	}

	// Check cluster health before proceeding with recovery
	// If a nodeclaim does have a nodepool label, validate the nodeclaims inside the nodepool are healthy (i.e below the allowed threshold)
	// In the case of standalone nodeclaim, validate the nodes inside the cluster are healthy before proceeding
	// to recover the nodes
	nodePoolName, found := nodeClaim.Labels[v1.NodePoolLabelKey]
	if found {
		nodePoolHealthy, err := c.isNodePoolHealthy(ctx, nodePoolName)
		if err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		if !nodePoolHealthy {
			return reconcile.Result{}, c.publishNodePoolHealthEvent(ctx, node, nodeClaim, nodePoolName)
		}
	} else {
		clusterHealthy, err := c.isClusterHealthy(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}
		if !clusterHealthy {
			c.recorder.Publish(NodeReadyTimeoutRecoveryBlockedUnmanagedNodeClaim(node, nodeClaim, fmt.Sprintf("more than %s nodes are unhealthy in the cluster", allowedUnhealthyPercent.String())))
			return reconcile.Result{}, nil
		}
	}

	// For nodes that have exceeded the ready timeout, trigger recovery
	if err := c.annotateTerminationGracePeriod(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return c.deleteNodeClaim(ctx, nodeClaim, node, nodeAge)
}

// deleteNodeClaim removes the NodeClaim from the api-server
func (c *Controller) deleteNodeClaim(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node, nodeAge time.Duration) (reconcile.Result, error) {
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// The deletion timestamp has successfully been set for the Node, update relevant metrics.
	log.FromContext(ctx).V(1).Info("deleting node due to ready timeout", "nodeAge", nodeAge.String())
	metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
		metrics.ReasonLabel:       metrics.ReadyTimeoutReason,
		metrics.NodePoolLabel:     node.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: node.Labels[v1.CapacityTypeLabelKey],
	})
	NodeReadyTimeoutRecoveredTotal.Inc(map[string]string{
		metrics.NodePoolLabel:     node.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: node.Labels[v1.CapacityTypeLabelKey],
	})
	c.recorder.Publish(NodeReadyTimeoutRecovery(node, nodeClaim, nodeAge))
	return reconcile.Result{}, nil
}

func (c *Controller) annotateTerminationGracePeriod(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	if expirationTimeString, exists := nodeClaim.ObjectMeta.Annotations[v1.NodeClaimTerminationTimestampAnnotationKey]; exists {
		expirationTime, err := time.Parse(time.RFC3339, expirationTimeString)
		if err == nil && expirationTime.Before(c.clock.Now()) {
			return nil
		}
	}
	stored := nodeClaim.DeepCopy()
	terminationTime := c.clock.Now().Format(time.RFC3339)
	nodeClaim.ObjectMeta.Annotations = lo.Assign(nodeClaim.ObjectMeta.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: terminationTime})

	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
			return err
		}
		log.FromContext(ctx).WithValues(v1.NodeClaimTerminationTimestampAnnotationKey, terminationTime).Info("annotated nodeclaim")
	}
	return nil
}

// isNodePoolHealthy checks if the number of unhealthy nodes managed by the given NodePool exceeds the health threshold.
// Up to 20% of Nodes may be unhealthy before the NodePool becomes unhealthy (or the nearest whole number, rounding up).
// For example, given a NodePool with three nodes, one may be unhealthy without rendering the NodePool unhealthy, even though that's 33% of the total nodes.
// This is analogous to how minAvailable and maxUnavailable work for PodDisruptionBudgets: https://kubernetes.io/docs/tasks/run-application/configure-pdb/#rounding-logic-when-specifying-percentages.
func (c *Controller) isNodePoolHealthy(ctx context.Context, nodePoolName string) (bool, error) {
	return c.areNodesHealthy(ctx, client.MatchingLabels(map[string]string{v1.NodePoolLabelKey: nodePoolName}))
}

func (c *Controller) isClusterHealthy(ctx context.Context) (bool, error) {
	return c.areNodesHealthy(ctx)
}

func (c *Controller) areNodesHealthy(ctx context.Context, opts ...client.ListOption) (bool, error) {
	nodeList := &corev1.NodeList{}
	if err := c.kubeClient.List(ctx, nodeList, append(opts, client.UnsafeDisableDeepCopy)...); err != nil {
		return false, err
	}
	unhealthyNodeCount := lo.CountBy(nodeList.Items, func(node corev1.Node) bool {
		readyCondition := nodeutils.GetCondition(lo.ToPtr(node), corev1.NodeReady)
		return readyCondition.Status != corev1.ConditionTrue
	})
	threshold := lo.Must(intstr.GetScaledValueFromIntOrPercent(lo.ToPtr(allowedUnhealthyPercent), len(nodeList.Items), true))
	return unhealthyNodeCount <= threshold, nil
}

func (c *Controller) publishNodePoolHealthEvent(ctx context.Context, node *corev1.Node, nodeClaim *v1.NodeClaim, npName string) error {
	np := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: npName}, np); err != nil {
		return client.IgnoreNotFound(err)
	}
	c.recorder.Publish(NodeReadyTimeoutRecoveryBlocked(node, nodeClaim, np, fmt.Sprintf("more than %s nodes are unhealthy in the nodepool", allowedUnhealthyPercent.String())))
	return nil
} 