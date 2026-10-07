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

// pollInterval is how often the controller re-evaluates unhealthy nodes. It bounds how long past its toleration an
// eligible node waits to be repaired.
const pollInterval = 15 * time.Second

// deletedTTL is how long a NodeClaim this controller deleted stays eligible for a termination deadline retry. It only
// needs to outlast the delete-to-stamp gap; the NodeClaim is terminating the whole time.
const deletedTTL = 10 * time.Minute

// Controller repairs registered nodes that are unhealthy before they initialize. The repair disruption method only
// considers initialized nodes and the NodeClaim liveness controller only considers unregistered ones, so without this
// controller a node that registers but never becomes healthy is stranded.
//
// Each pass re-evaluates the unhealthy nodes indexed in cluster state rather than reacting to Node and NodeClaim
// events. Eligibility depends on both objects, and several transitions change only one of them (registration labels the
// Node before it marks the NodeClaim Registered, and a reboot reaches its terminal outcome on the NodeClaim alone), so
// polling a consistent view avoids depending on which object's event arrives last.
//
// Disruption budgets don't apply: they exist to protect running workload, and they already exclude uninitialized nodes
// from both the node count and the in-flight disruptions. Repair still halts for a NodePool once more than
// health.UnhealthyThreshold of its nodes are unhealthy, so a correlated failure (e.g. a bad image) doesn't churn the pool.
type Controller struct {
	clock      clock.Clock
	kubeClient client.Client
	cluster    *state.Cluster
	recorder   events.Recorder
	matcher    *health.RepairPolicyMatcher
	// deleted holds the UIDs of NodeClaims this controller deleted, so it only stamps termination deadlines it owes.
	// Process-local: after a restart a missed stamp is not retried, which is safer than forcing a drain someone else owns.
	deleted *cache.Cache
}

// NewController constructs the controller around the matcher cluster state matches Nodes with, so cluster state, the
// repair disruption method, and this controller all use one compiled policy set. It panics when cluster state has none,
// since health.NewRepairPolicyMatcher only returns nil when node repair is disabled. Whatever action a matching policy
// selects, this controller terminates: the reboot controller bounds recovery from the Initialized condition's transition
// to Unknown, which never happens on a node that never initialized, so a reboot here would immediately time out and
// escalate to replacement anyway.
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

// Reconcile never returns an error. operatorpkg's reconciler drops RequeueAfter whenever one is returned, so the
// singleton would fall back to the default exponential rate limiter (up to ~17m) and one persistently failing node would
// stall repair for every other node. Each pass is idempotent and self-correcting, so failures are logged and retried on
// the next tick instead.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())
	if !options.FromContext(ctx).FeatureGates.NodeRepair {
		return reconciler.Result{RequeueAfter: pollInterval}, nil
	}
	// This doesn't wait for cluster state to sync: Synced is false while any NodeClaim is launching, so gating on it would
	// let launch churn, including the replacements repair itself causes, starve this controller. Every check is per node
	// and holds on a partial view: a Node whose NodeClaim isn't in state yet isn't Managed and is skipped, the delete is
	// preconditioned on the NodeClaim's ResourceVersion, and the breaker lists Nodes from the API server's cache.
	now := c.clock.Now()
	var tripped map[string]bool
	for _, node := range c.unhealthyNodes(now) {
		// A cheap pre-filter on cluster state's copy; repair re-reads and re-evaluates before acting on it.
		if _, ok := c.evaluate(node, now); !ok {
			continue
		}
		nodePoolName := node.NodeClaim.Labels[v1.NodePoolLabelKey]
		// The breaker lists every Node, so it only runs once per pass and only when a node is eligible.
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

// unhealthyNodes returns copies of the state nodes with a repair policy past its toleration at now. That is a superset of
// the nodes this controller acts on, since the reboot anchor only delays eligibility. Nodes holds cluster state's read
// lock while it yields, so this only copies inside the loop and leaves all API calls to the caller: calling back into
// cluster state mid-iteration could deadlock behind a waiting writer.
func (c *Controller) unhealthyNodes(now time.Time) state.StateNodes {
	var unhealthy state.StateNodes
	for node := range c.cluster.Nodes() {
		if node.GetRepairResult(now).Action != "" {
			unhealthy = append(unhealthy, node.DeepCopy())
		}
	}
	return unhealthy
}

// evaluate returns the node's repair decision and whether this controller should act on it: the node is NodePool-owned,
// registered but not initialized, not rebooting, not vetoed by do-not-repair, and has a policy past its toleration.
//
// A node counts as initialized when either the NodeClaim condition or the Node label says so: the initialization
// controller writes the label before the condition, and the repair disruption method keys off the label, so requiring
// both to be unset keeps the two repair paths disjoint. Rebooting nodes are left to the reboot controller, whose
// recovery deadline escalates to replacement. do-not-disrupt is deliberately ignored, as in the disruption method.
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

// rebootFinishedAt returns when the NodeClaim's most recent reboot reached a terminal outcome, or zero if it was never
// rebooted. The reboot controller's terminal write flips Rebooting from True to False, which stamps its transition time;
// evaluate excludes NodeClaims that are still rebooting, so any Rebooting condition seen here is terminal. Unhealthy time
// is only counted from then: a condition reported before or during the reboot (e.g. by an agent that hasn't re-reported
// since the node came back) says nothing about the rebooted node, so the node gets a full toleration to re-initialize,
// and return to the repair disruption method's reboot escalation, or clear the condition.
func rebootFinishedAt(nodeClaim *v1.NodeClaim) time.Time {
	rebooting := nodeClaim.StatusConditions(status.WithObservedOnly()).Get(v1.ConditionTypeRebooting)
	if rebooting == nil {
		return time.Time{}
	}
	return rebooting.LastTransitionTime.Time
}

// publishBlocked records that the circuit breaker is withholding repair from the node.
func (c *Controller) publishBlocked(ctx context.Context, node *state.StateNode, nodePoolName string) {
	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return
	}
	c.recorder.Publish(disruptionevents.NodeRepairBlocked(node.Node, node.NodeClaim, nodePool,
		fmt.Sprintf("more than %s of nodes in nodepool %q are unhealthy", health.UnhealthyThreshold, nodePoolName))...)
}

// repair re-reads the node and acts on it. Every decision is made on the fresh objects, because cluster state's copy can
// be stale and the Node half of the eligibility check is not covered by the delete precondition below: initialization
// labels the Node before it marks the NodeClaim Initialized, and that label write does not bump the NodeClaim's
// ResourceVersion.
func (c *Controller) repair(ctx context.Context, node *state.StateNode) error {
	fresh, ok, err := c.refresh(ctx, node)
	if err != nil || !ok {
		return err
	}
	result, eligible := c.evaluate(fresh, c.clock.Now())
	if !eligible {
		return nil
	}
	// Retry a termination deadline this controller already owes, when its delete succeeded but the stamp didn't. Only
	// deletions this controller initiated are stamped: tightening the deadline of a NodeClaim that something else is
	// deleting would convert its graceful drain into a forceful one.
	if !fresh.NodeClaim.DeletionTimestamp.IsZero() {
		if _, deletedHere := c.deleted.Get(string(fresh.NodeClaim.UID)); deletedHere {
			return c.stampTerminationDeadline(ctx, fresh.NodeClaim)
		}
		return nil
	}
	return c.delete(ctx, fresh, result)
}

// refresh re-reads the state node's Node and NodeClaim from the API, returning false when either is gone or when the
// Node's repair-policy inputs changed since cluster state matched them, so the cached matches can't be stale.
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

// delete requests NodeClaim deletion, preconditioned on the ResourceVersion just read so a NodeClaim that changed in the
// meantime is re-evaluated on a later pass instead of deleted. Termination is forceful, matching v1.14 node repair:
// drainable pods are force-deleted with a minimal grace period, bypassing PDBs and do-not-disrupt. That includes workload
// pods, which can be present on a node that never initialized (e.g. one that is Ready but whose requested extended
// resource never registers, or one that stalls re-initializing after a reboot). The deadline is stamped only after the
// delete succeeds: stamping first would bump the ResourceVersion the delete is preconditioned on, and would leave a stale
// deadline on a NodeClaim that escapes repair.
func (c *Controller) delete(ctx context.Context, fresh *state.StateNode, result health.RepairResult) error {
	nodeClaim := fresh.NodeClaim
	if err := c.kubeClient.Delete(ctx, nodeClaim, client.Preconditions{ResourceVersion: lo.ToPtr(nodeClaim.ResourceVersion)}); err != nil {
		// A conflict means cluster state is behind; the next pass re-evaluates the current object.
		if errors.IsConflict(err) || errors.IsNotFound(err) {
			return nil
		}
		return serrors.Wrap(fmt.Errorf("deleting nodeclaim, %w", err), "NodeClaim", klog.KObj(nodeClaim))
	}
	// Recorded before the stamp, so a failed stamp is retried on a later pass.
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
	// Errors don't fail the pass; the metric reports 0.
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

// stampTerminationDeadline tightens the NodeClaim's termination deadline to now. It never extends an earlier deadline.
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
