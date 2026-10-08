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

package uninitialized

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/awslabs/operatorpkg/status"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

const pollInterval = 15 * time.Second

// deletedTTL bounds how long a failed termination deadline stamp is retried.
const deletedTTL = 10 * time.Minute

// Controller replaces registered nodes that are unhealthy and never initialized, which neither the repair disruption
// method (initialized nodes) nor NodeClaim liveness (unregistered nodes) handles. It polls cluster state because
// eligibility spans the Node and NodeClaim, which change independently. Disruption budgets don't apply, since they
// exclude uninitialized nodes; the unhealthy-node circuit breaker does.
type Controller struct {
	clock      clock.Clock
	kubeClient client.Client
	cluster    *state.Cluster
	recorder   events.Recorder
	matcher    *health.RepairPolicyMatcher
	// deleted holds the UIDs of NodeClaims this controller deleted, so it never forces a drain something else started.
	deleted *cache.Cache
}

// NewController panics if cluster state has no repair policy matcher. Every policy action is treated as a replacement: a
// reboot times out against the Initialized transition, which a node that never initialized doesn't have.
func NewController(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, recorder events.Recorder) *Controller {
	matcher := cluster.RepairPolicyMatcher()
	if matcher == nil {
		panic("uninitialized node repair requires cluster state built with a repair policy matcher")
	}
	return &Controller{
		clock:      clk,
		kubeClient: kubeClient,
		cluster:    cluster,
		recorder:   recorder,
		matcher:    matcher,
		deleted:    cache.New(deletedTTL, time.Minute),
	}
}

func (c *Controller) Name() string {
	return "node.health.uninitialized"
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.health.uninitialized").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

// Reconcile logs per-node errors instead of returning them, since returning one drops RequeueAfter and backs off the
// whole loop.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())
	if !options.FromContext(ctx).FeatureGates.NodeRepair {
		return reconciler.Result{RequeueAfter: pollInterval}, nil
	}
	// Don't wait on cluster.Synced: it's false while any NodeClaim launches, and every check here is per node.
	now := c.clock.Now()
	var tripped map[string]bool
	for _, node := range c.unhealthyNodes(now) {
		nodePoolName := node.NodeClaim.Labels[v1.NodePoolLabelKey]
		// The breaker lists every Node, so compute it at most once per pass.
		if tripped == nil {
			var err error
			if tripped, err = health.TrippedNodePools(ctx, c.kubeClient, c.matcher); err != nil {
				log.FromContext(ctx).Error(err, "determining unhealthy nodepools")
				return reconciler.Result{RequeueAfter: pollInterval}, nil
			}
		}
		if tripped[nodePoolName] {
			c.publishBlocked(ctx, node, nodePoolName)
			continue
		}
		ctx := log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KObj(node.Node), "NodeClaim", klog.KObj(node.NodeClaim)))
		if err := c.repair(ctx, node); err != nil {
			log.FromContext(ctx).Error(err, "repairing unhealthy node that never initialized")
		}
	}
	return reconciler.Result{RequeueAfter: pollInterval}, nil
}

// unhealthyNodes copies the nodes this controller would act on. Nodes holds cluster state's read lock, so only nodes
// that pass the read-only checks are copied, and nothing else is done inside the loop.
func (c *Controller) unhealthyNodes(now time.Time) state.StateNodes {
	var unhealthy state.StateNodes
	for node := range c.cluster.Nodes() {
		if node.GetRepairResult(now).Action == "" {
			continue
		}
		if _, ok := c.evaluate(node, now); ok {
			unhealthy = append(unhealthy, node.DeepCopy())
		}
	}
	return unhealthy
}

// evaluate returns the node's repair decision and whether this controller owns it. The initialized label and condition
// are both checked because initialization writes the label first, and the repair disruption method reads the label.
func (c *Controller) evaluate(node *state.StateNode, now time.Time) (health.RepairResult, bool) {
	if !node.Managed() || node.Node == nil || node.NodeClaim.Labels[v1.NodePoolLabelKey] == "" {
		return health.RepairResult{}, false
	}
	conditions := node.NodeClaim.StatusConditions(status.WithObservedOnly())
	if !conditions.Get(v1.ConditionTypeRegistered).IsTrue() ||
		conditions.Get(v1.ConditionTypeInitialized).IsTrue() ||
		node.Node.Labels[v1.NodeInitializedLabelKey] == "true" ||
		conditions.Get(v1.ConditionTypeRebooting).IsTrue() {
		return health.RepairResult{}, false
	}
	if node.Node.Annotations[v1.DoNotRepairAnnotationKey] == "true" || node.NodeClaim.Annotations[v1.DoNotRepairAnnotationKey] == "true" {
		return health.RepairResult{}, false
	}
	result := node.GetRepairResultSince(now, rebootFinishedAt(node.NodeClaim))
	return result, result.Action != ""
}

// rebootFinishedAt returns when the last reboot finished, so unhealthy time from before or during it isn't counted.
func rebootFinishedAt(nodeClaim *v1.NodeClaim) time.Time {
	rebooting := nodeClaim.StatusConditions(status.WithObservedOnly()).Get(v1.ConditionTypeRebooting)
	if rebooting == nil {
		return time.Time{}
	}
	return rebooting.LastTransitionTime.Time
}

func (c *Controller) publishBlocked(ctx context.Context, node *state.StateNode, nodePoolName string) {
	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return
	}
	c.recorder.Publish(disruptionevents.NodeRepairBlocked(node.Node, node.NodeClaim, nodePool,
		fmt.Sprintf("more than %s of nodes in nodepool %q are unhealthy", health.UnhealthyThreshold, nodePoolName))...)
}

// repair decides on freshly read objects, since cluster state can be stale and the delete precondition only covers the
// NodeClaim.
func (c *Controller) repair(ctx context.Context, node *state.StateNode) error {
	fresh, ok, err := c.refresh(ctx, node)
	if err != nil || !ok {
		return err
	}
	result, eligible := c.evaluate(fresh, c.clock.Now())
	if !eligible {
		return nil
	}
	// Retry a stamp that failed after our own delete.
	if !fresh.NodeClaim.DeletionTimestamp.IsZero() {
		if _, deletedHere := c.deleted.Get(string(fresh.NodeClaim.UID)); deletedHere {
			return c.stampTerminationDeadline(ctx, fresh.NodeClaim)
		}
		return nil
	}
	return c.delete(ctx, fresh, result)
}

// refresh re-reads the Node and NodeClaim, returning false if either is gone or the cached policy matches are stale.
func (c *Controller) refresh(ctx context.Context, node *state.StateNode) (*state.StateNode, bool, error) {
	nodeClaim := &v1.NodeClaim{}
	if err := c.kubeClient.Get(ctx, client.ObjectKeyFromObject(node.NodeClaim), nodeClaim); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	freshNode := &corev1.Node{}
	if err := c.kubeClient.Get(ctx, client.ObjectKeyFromObject(node.Node), freshNode); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	if !health.MatchInputsEqual(node.Node, freshNode) {
		return nil, false, nil
	}
	fresh := node.ShallowCopy()
	fresh.Node = freshNode
	fresh.NodeClaim = nodeClaim
	return fresh, true, nil
}

// delete forcefully terminates the NodeClaim, as v1.14 node repair did. The deadline is stamped after the delete so a
// failed delete never leaves one behind.
func (c *Controller) delete(ctx context.Context, fresh *state.StateNode, result health.RepairResult) error {
	nodeClaim := fresh.NodeClaim
	if err := c.kubeClient.Delete(ctx, nodeClaim, client.Preconditions{ResourceVersion: lo.ToPtr(nodeClaim.ResourceVersion)}); err != nil {
		if errors.IsConflict(err) || errors.IsNotFound(err) {
			return nil
		}
		return serrors.Wrap(fmt.Errorf("deleting nodeclaim, %w", err), "NodeClaim", klog.KObj(nodeClaim))
	}
	c.deleted.SetDefault(string(nodeClaim.UID), struct{}{})
	log.FromContext(ctx).WithValues(
		"condition", result.Condition,
		"status", result.ConditionStatus,
		"reason", result.Reason,
		"eligible-at", result.SelectedEligibleAt,
	).Info("deleting unhealthy node that never initialized")
	labels := map[string]string{
		metrics.ReasonLabel:              metrics.UnhealthyReason,
		metrics.NodePoolLabel:            nodeClaim.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel:        nodeClaim.Labels[v1.CapacityTypeLabelKey],
		metrics.ConsolidationPolicyLabel: "",
		metrics.TerminationModeLabel:     metrics.TerminationModeForceful,
	}
	metrics.NodeClaimsDisruptedTotal.Inc(labels)
	reschedulablePods, err := nodeutils.ReschedulablePods(ctx, c.kubeClient, fresh.Node.Name)
	if err != nil {
		log.FromContext(ctx).V(1).Info("listing reschedulable pods for disruption metric", "error", err.Error())
	}
	metrics.PodsDisruptionInitiatedTotal.Add(float64(len(reschedulablePods)), labels)
	health.NodeClaimsUnhealthyDisruptedTotal.Inc(map[string]string{
		health.ConditionLabel:        pretty.ToSnakeCase(string(result.Condition)),
		metrics.NodePoolLabel:        nodeClaim.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel:    nodeClaim.Labels[v1.CapacityTypeLabelKey],
		health.ImageIDLabel:          nodeClaim.Status.ImageID,
		metrics.TerminationModeLabel: metrics.TerminationModeForceful,
	})
	return c.stampTerminationDeadline(ctx, nodeClaim)
}

// stampTerminationDeadline tightens the termination deadline to now, never extending an earlier one.
func (c *Controller) stampTerminationDeadline(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	deadline := c.clock.Now()
	return retry.OnError(retry.DefaultBackoff, errors.IsConflict, func() error {
		stored := &v1.NodeClaim{}
		if err := c.kubeClient.Get(ctx, client.ObjectKeyFromObject(nodeClaim), stored); err != nil {
			return client.IgnoreNotFound(err)
		}
		if value, ok := stored.Annotations[v1.NodeClaimTerminationTimestampAnnotationKey]; ok {
			if existing, err := time.Parse(time.RFC3339, value); err == nil && !existing.After(deadline) {
				return nil
			}
		}
		if err := nodeclaimutils.PatchTerminationTimestampAnnotation(ctx, c.kubeClient, stored, deadline); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil
			}
			return serrors.Wrap(fmt.Errorf("patching termination timestamp, %w", err), "NodeClaim", klog.KObj(nodeClaim))
		}
		return nil
	})
}
