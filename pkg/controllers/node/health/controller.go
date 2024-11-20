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

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

var allowedUnhealthyPercent = intstr.FromString("20%")

// Controller for the resource
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, clock clock.Clock) *Controller {
	return &Controller{
		clock:         clock,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.health").
		For(&corev1.Node{}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func (c *Controller) Reconcile(ctx context.Context, node *corev1.Node) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "node.health")
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KRef(node.Namespace, node.Name)))

	// Validate that the node is owned by us
	nodeClaim, err := nodeutils.NodeClaimForNode(ctx, c.kubeClient, node)
	if err != nil {
		return reconcile.Result{}, nodeutils.IgnoreNodeClaimNotFoundError(err)
	}

	nodePoolName, found := nodeClaim.Labels[v1.NodePoolLabelKey]
	if found {
		nodePoolHealthy, err := c.isNodePoolHealthy(ctx, nodePoolName)
		if err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		if !nodePoolHealthy {
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		}
	} else {
		clusterHealthy, err := c.isClusterHealthy(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}
		if !clusterHealthy {
			log.FromContext(ctx).V(1).Info(fmt.Sprintf("more then %s nodes are unhealthy", allowedUnhealthyPercent.String()))
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		}
	}

	unhealthyNodeCondition, policyTerminationDuration := c.findUnhealthyConditions(node)
	if unhealthyNodeCondition == nil {
		return reconcile.Result{}, nil
	}

	// If the Node is unhealthy, but has not reached it's full toleration disruption
	// requeue at the termination time of the unhealthy node
	terminationTime := unhealthyNodeCondition.LastTransitionTime.Add(policyTerminationDuration)
	if c.clock.Now().Before(terminationTime) {
		return reconcile.Result{RequeueAfter: terminationTime.Sub(c.clock.Now())}, nil
	}

	// For unhealthy past the tolerationDisruption window we can forcefully terminate the node
	if err := c.annotateTerminationGracePeriod(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// The deletion timestamp has successfully been set for the Node, update relevant metrics.
	log.FromContext(ctx).V(1).Info("deleting unhealthy node")
	metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
		metrics.ReasonLabel:       string(unhealthyNodeCondition.Type),
		metrics.NodePoolLabel:     node.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: node.Labels[v1.CapacityTypeLabelKey],
	})
	return reconcile.Result{}, nil
}

// Find a node with a condition that matches one of the unhealthy conditions defined by the cloud provider
// If there are multiple unhealthy status condition we will requeue based on the condition closest to its terminationDuration
func (c *Controller) findUnhealthyConditions(node *corev1.Node) (nc *corev1.NodeCondition, cpTerminationDuration time.Duration) {
	requeueTime := time.Time{}
	for _, policy := range c.cloudProvider.RepairPolicies() {
		// check the status and the type on the condition
		nodeCondition := nodeutils.GetCondition(node, policy.ConditionType)
		if nodeCondition.Status == policy.ConditionStatus {
			terminationTime := nodeCondition.LastTransitionTime.Add(policy.TolerationDuration)
			// Determine requeue time
			if requeueTime.IsZero() || requeueTime.After(terminationTime) {
				nc = lo.ToPtr(nodeCondition)
				cpTerminationDuration = policy.TolerationDuration
				requeueTime = terminationTime
			}
		}
	}
	return nc, cpTerminationDuration
}

func (c *Controller) annotateTerminationGracePeriod(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	stored := nodeClaim.DeepCopy()
	nodeClaim.ObjectMeta.Annotations = lo.Assign(nodeClaim.ObjectMeta.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: c.clock.Now().Format(time.RFC3339)})

	if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
		return err
	}

	return nil
}

// isNodePoolHealthy checks if the number of unhealthy nodes managed by the given NodePool exceeds the health threshold.
// defined by the cloud provider
// Up to 20% of Nodes may be unhealthy before the NodePool becomes unhealthy (or the nearest whole number, rounding up).
// For example, given a NodePool with three nodes, one may be unhealthy without rendering the NodePool unhealthy, even though that's 33% of the total nodes.
// This is analogous to how minAvailable and maxUnavailable work for PodDisruptionBudgets: https://kubernetes.io/docs/tasks/run-application/configure-pdb/#rounding-logic-when-specifying-percentages.
func (c *Controller) isNodePoolHealthy(ctx context.Context, nodePoolName string) (bool, error) {
	nodeList := &corev1.NodeList{}
	if err := c.kubeClient.List(ctx, nodeList, client.MatchingLabels(map[string]string{v1.NodePoolLabelKey: nodePoolName})); err != nil {
		return false, err
	}
	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return false, err
	}
	stored := nodePool.DeepCopy()

	healthy := c.validateHealthyCloudProviderHealthCondition(nodeList.Items)
	if !healthy {
		nodePool.StatusConditions().SetTrueWithReason(v1.ConditionTypeUnhealthy, "Unhealthy", fmt.Sprintf("more then %s nodes are unhealthy", allowedUnhealthyPercent.String()))
	} else {
		nodePool.StatusConditions().Clear(v1.ConditionTypeUnhealthy)
	}

	if !equality.Semantic.DeepEqual(stored, nodePool) {
		if err := c.kubeClient.Patch(ctx, nodePool, client.MergeFrom(stored)); err != nil {
			return false, err
		}
	}

	return true, nil
}

func (c *Controller) isClusterHealthy(ctx context.Context) (bool, error) {
	nodeList := &corev1.NodeList{}
	if err := c.kubeClient.List(ctx, nodeList); err != nil {
		return false, err
	}

	return c.validateHealthyCloudProviderHealthCondition(nodeList.Items), nil
}

func (c *Controller) validateHealthyCloudProviderHealthCondition(nodes []corev1.Node) bool {
	for _, policy := range c.cloudProvider.RepairPolicies() {
		unhealthyNodeCount := lo.CountBy(nodes, func(node corev1.Node) bool {
			nodeCondition := nodeutils.GetCondition(lo.ToPtr(node), policy.ConditionType)
			return nodeCondition.Status == policy.ConditionStatus
		})

		threshold := lo.Must(intstr.GetScaledValueFromIntOrPercent(lo.ToPtr(allowedUnhealthyPercent), len(nodes), true))
		if unhealthyNodeCount > threshold {
			return false
		}
	}
	return true
}
