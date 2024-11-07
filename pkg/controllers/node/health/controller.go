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

	// Validate that the node is owned by us and is not being deleted
	nodeClaim, err := nodeutils.NodeClaimForNode(ctx, c.kubeClient, node)
	if err != nil {
		return reconcile.Result{}, nodeutils.IgnoreNodeClaimNotFoundError(err)
	}

	log.FromContext(ctx).V(1).Info("found nodeclaim")
	// If find if a node is unhealthy
	healthCondition, foundHealthCondition := lo.Find(c.cloudProvider.RepairPolicies(), func(policy cloudprovider.RepairPolicy) bool {
		nodeCondition := nodeutils.GetCondition(node, policy.ConditionType)
		for _, condition := range node.Status.Conditions {
			if condition.Type == policy.ConditionType && nodeCondition.Status == policy.ConditionStatus {
				terminationTime := condition.LastTransitionTime.Add(policy.TolerationDuration)
				return !c.clock.Now().Before(terminationTime)
			}
		}
		return false
	})

	log.FromContext(ctx).V(1).Info(fmt.Sprintf("found status: %v", foundHealthCondition))
	// If the Node is unhealthy, but has not reached it's full toleration disruption
	// requeue at the termination time of the unhealthy node
	terminationTime := nodeutils.GetCondition(node, healthCondition.ConditionType).LastTransitionTime.Add(healthCondition.TolerationDuration)
	log.FromContext(ctx).V(1).Info(fmt.Sprintf("time now: %s, terminate: %s", c.clock.Now().String(), terminationTime.String()))
	if !foundHealthCondition {
		log.FromContext(ctx).V(1).Info("termination time")
		return reconcile.Result{RequeueAfter: terminationTime.Sub(c.clock.Now())}, nil
	}

	// For unhealthy past the tolerationDisruption window we can forcefully terminate the node
	if err := c.annotateTerminationGracePeriod(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, err
	}

	// The deletion timestamp has successfully been set for the Node, update relevant metrics.
	log.FromContext(ctx).V(1).Info("deleting unhealthy node")
	metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
		metrics.ReasonLabel:       string(healthCondition.ConditionType),
		metrics.NodePoolLabel:     node.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: node.Labels[v1.CapacityTypeLabelKey],
	})
	return reconcile.Result{}, nil
}

func (c *Controller) annotateTerminationGracePeriod(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	stored := nodeClaim.DeepCopy()
	nodeClaim.ObjectMeta.Annotations = lo.Assign(nodeClaim.ObjectMeta.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: c.clock.Now().Format(time.RFC3339)})

	if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
		return err
	}

	return nil
}
