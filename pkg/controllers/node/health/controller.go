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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
	nodeClaim, err := nodeutils.NodeClaimForNode(ctx, c.kubeClient, node)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting nodeclaim, %w", err)
	}

	ctx = injection.WithControllerName(ctx, "node.health")
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("nodeclaim", nodeClaim.Name))
	nodeHealthCondition := corev1.NodeCondition{}
	foundCloudProviderPolicy := cloudprovider.RepairStatement{}

	// Validate that the node is owned by us and is not being deleted
	if !node.GetDeletionTimestamp().IsZero() || !controllerutil.ContainsFinalizer(node, v1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}

	for _, policy := range c.cloudProvider.RepairPolicy() {
		nodeHealthCondition = nodeutils.GetCondition(node, policy.Type)
		if nodeHealthCondition.Status == policy.Status {
			// found unhealthy condition on the node
			foundCloudProviderPolicy = policy
			break
		}
	}

	// From here there are three scenarios to handle:
	// 1. If node is healthy, exit node repair loop
	if foundCloudProviderPolicy.Type == "" {
		return reconcile.Result{}, nil
	}

	// 2. If the Node is unhealthy, but has not reached it's full toleration disruption, exit the loop
	disruptionTime := nodeHealthCondition.LastTransitionTime.Add(foundCloudProviderPolicy.TolerationDuration)
	if c.clock.Now().Before(disruptionTime) {
		return reconcile.Result{RequeueAfter: disruptionTime.Sub(c.clock.Now())}, nil
	}

	if err := c.annotateTerminationGracePeriod(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, fmt.Errorf("annotated termination grace period on nodeclaim, %w", err)
	}

	// 3. Otherwise, if the Node is unhealthy and past it's tolerationDisruption window we can forcefully terminate the node
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// 4. The deletion timestamp has successfully been set for the Node, update relevant metrics.
	log.FromContext(ctx).V(1).Info("deleting unhealthy node")
	metrics.NodeClaimsDisruptedTotal.With(prometheus.Labels{
		metrics.ReasonLabel:       metrics.UnhealthyReason,
		metrics.NodePoolLabel:     nodeClaim.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: nodeClaim.Labels[v1.CapacityTypeLabelKey],
	}).Inc()
	return reconcile.Result{}, nil
}

func (c *Controller) annotateTerminationGracePeriod(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	stored := nodeClaim.DeepCopy()
	terminationTime := c.clock.Now().Format(time.RFC3339)
	nodeClaim.ObjectMeta.Annotations = lo.Assign(nodeClaim.ObjectMeta.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: terminationTime})

	if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
		return client.IgnoreNotFound(err)
	}

	return nil
}
