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

package counter

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/awslabs/operatorpkg/status"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// Controller for the resource
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster
	clock         clock.Clock
	recorder      events.Recorder
}

var BaseResources = corev1.ResourceList{
	corev1.ResourceCPU:              resource.MustParse("0"),
	corev1.ResourceMemory:           resource.MustParse("0"),
	corev1.ResourcePods:             resource.MustParse("0"),
	corev1.ResourceEphemeralStorage: resource.MustParse("0"),
	resources.Node:                  resource.MustParse("0"),
}

// NewController is a constructor
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster, clock clock.Clock, recorder events.Recorder) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		cluster:       cluster,
		clock:         clock,
		recorder:      recorder,
	}
}

// Reconcile a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, nodePool *v1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodepool.counter")
	if !nodepoolutils.IsManaged(nodePool, c.cloudProvider) {
		return reconcile.Result{}, nil
	}

	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// Otherwise, we have the potential to patch over the status with a lower value for the nodepool resource
	// counts on startup
	if !c.cluster.Synced(ctx) {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	stored := nodePool.DeepCopy()

	// Track disruption count and reasons
	currentDisruptions := 0
	disruptionReasons := map[v1.DisruptionReason]int{}
	c.cluster.ForEachNode(func(n *state.StateNode) bool {
		if !n.Managed() || !n.Initialized() {
			return true
		}

		// we don't count nodes that are in terminating status since cloud provider has already initiated termination
		// after disruption
		if n.NodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue() {
			return true
		}

		if n.Labels()[v1.NodePoolLabelKey] == nodePool.Name {
			if cond, found := lo.Find(n.NodeClaim.GetConditions(), func(s status.Condition) bool {
				return s.Type == v1.ConditionTypeDisruptionReason && s.GetStatus() == metav1.ConditionTrue
			}); found {
				currentDisruptions++
				disruptionReasons[v1.DisruptionReason(cond.Reason)]++
			}
		}
		// continue with our iteration
		return true
	})

	// Build budget mapping for each reason that has disrupted nodes
	var totalAllowedDisruptions int
	for reason := range disruptionReasons {
		disruptionBudgetMapping, err := disruption.BuildDisruptionBudgetMapping(ctx, c.cluster, c.clock, c.kubeClient, c.cloudProvider, c.recorder, reason)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("building disruption budgets for reason %s, %w", reason, err)
		}
		totalAllowedDisruptions += disruptionBudgetMapping[nodePool.Name]
	}

	// Update the NodePool's status with disruption information
	nodePool.Status.Disruptions = v1.DisruptionStatus{
		AllowedDisruptionCount: totalAllowedDisruptions,
		CurrentDisruptionCount: currentDisruptions,
		DisruptionReasonCount:  disruptionReasons,
	}

	// Determine disruption status, resource usage and update nodepool.status.resources
	nodePool.Status.Resources = lo.Assign(BaseResources, c.cluster.NodePoolResourcesFor(nodePool.Name))
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		if err := c.kubeClient.Status().Patch(ctx, nodePool, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{RequeueAfter: time.Second * 5}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodepool.counter").
		For(&v1.NodePool{}, builder.WithPredicates(nodepoolutils.IsManagedPredicateFuncs(c.cloudProvider), predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return true },
			UpdateFunc: func(e event.UpdateEvent) bool { return false },
			DeleteFunc: func(e event.DeleteEvent) bool { return false },
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))

}
