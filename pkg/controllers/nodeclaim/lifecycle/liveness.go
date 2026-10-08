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
	"time"

	"github.com/awslabs/operatorpkg/object"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolhealth"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

type Liveness struct {
	clock      clock.Clock
	kubeClient client.Client
	cluster    *state.Cluster
	recorder   events.Recorder
	npState    *nodepoolhealth.State
}

// registrationTimeout is a heuristic time that we expect the node to register within
// If we don't see the node within this time, then we should delete the NodeClaim and try again

const (
	registrationTimeout = time.Minute * 15
	// Sourced from pkg/metrics so the documented reason values stay in one place.
	registrationTimeoutReason = metrics.RegistrationTimeoutReason
	launchTimeoutReason       = metrics.LaunchTimeoutReason
)

// unhealthyRepairMessage distinguishes liveness's DisruptionReason mark from the repair disruption method's, which shares
// its reason, so finalization only forces the drain of NodeClaims liveness is replacing.
const unhealthyRepairMessage = "Replacing an unhealthy node that never initialized"

// unhealthyBlockedRequeue is how often a NodeClaim blocked by the unhealthy-node circuit breaker is re-checked.
const unhealthyBlockedRequeue = time.Minute

// LaunchTimeout is a heuristic time that we expect to be able to launch within
// If we don't launch within this time, then we should delete the NodeClaim and try again
var LaunchTimeout = time.Minute * 5

//nolint:gocyclo
func (l *Liveness) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	registered := nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered)
	if registered.IsTrue() {
		return l.reconcileUnhealthy(ctx, nodeClaim)
	}
	launched := nodeClaim.StatusConditions().Get(v1.ConditionTypeLaunched)
	if launched == nil {
		return reconcile.Result{Requeue: true}, nil
	}
	if !launched.IsTrue() {
		if timeUntilTimeout := LaunchTimeout - l.clock.Since(launched.LastTransitionTime.Time); timeUntilTimeout > 0 {
			// This should never occur because if we failed to launch we requeue the object with error instead of this requeueAfter
			return reconcile.Result{RequeueAfter: timeUntilTimeout}, nil
		}
		if err := l.updateNodePoolRegistrationHealth(ctx, nodeClaim); client.IgnoreNotFound(err) != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, err
		}
		if err := l.deleteNodeClaimForTimeout(ctx, LaunchTimeout, launchTimeoutReason, nodeClaim); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{}, nil
		}
	}
	if registered == nil {
		return reconcile.Result{Requeue: true}, nil
	}
	// If the Registered statusCondition hasn't gone True during the timeout since we first updated it, we should terminate the NodeClaim
	// NOTE: Timeout has to be stored and checked in the same place since l.clock can advance after the check causing a race
	if timeUntilTimeout := registrationTimeout - l.clock.Since(registered.LastTransitionTime.Time); timeUntilTimeout > 0 {
		return reconcile.Result{RequeueAfter: timeUntilTimeout}, nil
	}
	if err := l.updateNodePoolRegistrationHealth(ctx, nodeClaim); client.IgnoreNotFound(err) != nil {
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	// Delete the NodeClaim if we believe the NodeClaim won't register since we haven't seen the node
	if err := l.deleteNodeClaimForTimeout(ctx, registrationTimeout, registrationTimeoutReason, nodeClaim); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, nil
}

// updateNodePoolRegistrationHealth sets the NodeRegistrationHealthy=False
// on the NodePool if the nodeClaim fails to launch/register
func (l *Liveness) updateNodePoolRegistrationHealth(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	nodePoolName := nodeClaim.Labels[v1.NodePoolLabelKey]
	if nodePoolName != "" {
		nodePool := &v1.NodePool{}
		if err := l.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
			return serrors.Wrap(fmt.Errorf("getting nodepool, %w", err), "NodePool", klog.KRef("", nodePoolName))
		}
		if _, found := lo.Find(nodeClaim.GetOwnerReferences(), func(o metav1.OwnerReference) bool {
			return o.Kind == object.GVK(nodePool).Kind && o.UID == nodePool.UID
		}); !found {
			return nil
		}
		stored := nodePool.DeepCopy()
		if l.npState.DryRun(nodePool.UID, false).Status() == nodepoolhealth.StatusUnhealthy && !nodePool.StatusConditions().Get(v1.ConditionTypeNodeRegistrationHealthy).IsFalse() {
			// If the nodeClaim failed to register during the timeout set NodeRegistrationHealthy status condition on
			// NodePool to False. If the launch failed get the launch failure reason and message from nodeClaim.
			if launchCondition := nodeClaim.StatusConditions().Get(v1.ConditionTypeLaunched); launchCondition.IsTrue() {
				nodePool.StatusConditions(status.WithClock(l.clock)).SetFalse(v1.ConditionTypeNodeRegistrationHealthy, "RegistrationFailed", "Failed to register node")
			} else {
				nodePool.StatusConditions(status.WithClock(l.clock)).SetFalse(v1.ConditionTypeNodeRegistrationHealthy, launchCondition.Reason, launchCondition.Message)
			}
			// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
			// can cause races due to the fact that it fully replaces the list on a change
			// Here, we are updating the status condition list
			if err := l.kubeClient.Status().Patch(ctx, nodePool, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
				return serrors.Wrap(fmt.Errorf("patching nodepool status, %w", err), "NodePool", klog.KObj(nodePool))
			}
		}
		l.npState.Update(nodePool.UID, false)
	}
	return nil
}

func (l *Liveness) deleteNodeClaimForTimeout(ctx context.Context, timeout time.Duration, reason string, nodeClaim *v1.NodeClaim) error {
	if err := l.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return serrors.Wrap(fmt.Errorf("deleting nodeclaim for timeout, %w", err), "NodeClaim", klog.KObj(nodeClaim))
	}
	log.FromContext(ctx).V(1).WithValues("timeout", timeout, "reason", reason).Info("terminating due to timeout")
	metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
		metrics.ReasonLabel:              reason,
		metrics.NodePoolLabel:            nodeClaim.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel:        nodeClaim.Labels[v1.CapacityTypeLabelKey],
		metrics.ConsolidationPolicyLabel: "",
		metrics.TerminationModeLabel:     nodeclaimutils.DisruptionTerminationMode(nodeClaim),
	})
	return nil
}

// reconcileUnhealthy forcefully replaces a registered NodeClaim whose Node never initialized and has been unhealthy past a
// repair policy's toleration. The repair disruption method only acts on initialized nodes, and disruption budgets exclude
// uninitialized ones, so only the unhealthy-node circuit breaker applies. Every policy action is treated as a
// replacement: a reboot times out against the Initialized transition, which a node that never initialized doesn't have.
func (l *Liveness) reconcileUnhealthy(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	res, deleteAttempted, err := l.repairUnhealthy(ctx, nodeClaim)
	// A mark left by a delete that failed would otherwise force the drain of a later, unrelated deletion.
	if !deleteAttempted && markedForUnhealthyRepair(nodeClaim) {
		_ = nodeClaim.StatusConditions(status.WithClock(l.clock)).Clear(v1.ConditionTypeDisruptionReason)
	}
	return res, err
}

func (l *Liveness) repairUnhealthy(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, bool, error) {
	node, matcher, err := l.unhealthyRepairCandidate(ctx, nodeClaim)
	if node == nil || err != nil {
		return reconcile.Result{}, false, err
	}
	now := l.clock.Now()
	matches := matcher.Match(node)
	notBefore := rebootFinishedAt(nodeClaim)
	result := health.ResolveSince(matches, now, notBefore)
	if result.Action == "" {
		// Node updates requeue the NodeClaim as its conditions change; only the toleration needs a timer.
		if next, ok := health.NextEligibleAt(matches, now, notBefore); ok {
			return reconcile.Result{RequeueAfter: next.Sub(now)}, false, nil
		}
		return reconcile.Result{}, false, nil
	}
	nodePoolName := nodeClaim.Labels[v1.NodePoolLabelKey]
	tripped, err := health.TrippedNodePools(ctx, l.kubeClient, matcher)
	if err != nil {
		return reconcile.Result{}, false, serrors.Wrap(fmt.Errorf("determining unhealthy nodepools, %w", err), "NodePool", klog.KRef("", nodePoolName))
	}
	if tripped[nodePoolName] {
		l.publishRepairBlocked(ctx, nodeClaim, node, nodePoolName)
		return reconcile.Result{RequeueAfter: unhealthyBlockedRequeue}, false, nil
	}
	return reconcile.Result{}, true, l.deleteUnhealthy(ctx, nodeClaim, node, result)
}

// unhealthyRepairCandidate returns the NodeClaim's Node and the repair policy matcher when liveness owns repairing it,
// or a nil Node when it doesn't.
func (l *Liveness) unhealthyRepairCandidate(ctx context.Context, nodeClaim *v1.NodeClaim) (*corev1.Node, *health.RepairPolicyMatcher, error) {
	matcher := l.cluster.RepairPolicyMatcher()
	if !options.FromContext(ctx).FeatureGates.NodeRepair || matcher == nil || !ownsUnhealthyRepair(nodeClaim) ||
		nodeClaim.Annotations[v1.DoNotRepairAnnotationKey] == "true" {
		return nil, nil, nil
	}
	node, err := nodeclaimutils.NodeForNodeClaim(ctx, l.kubeClient, nodeClaim)
	if err != nil {
		return nil, nil, nodeclaimutils.IgnoreNodeNotFoundError(nodeclaimutils.IgnoreDuplicateNodeError(err))
	}
	// Initialization labels the Node before it marks the NodeClaim, and the repair disruption method reads the label.
	if node.Labels[v1.NodeInitializedLabelKey] == "true" || node.Annotations[v1.DoNotRepairAnnotationKey] == "true" {
		return nil, nil, nil
	}
	return node, matcher, nil
}

func (l *Liveness) publishRepairBlocked(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node, nodePoolName string) {
	nodePool := &v1.NodePool{}
	if err := l.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return
	}
	l.recorder.Publish(disruptionevents.NodeRepairBlocked(node, nodeClaim, nodePool,
		fmt.Sprintf("more than %s of nodes in nodepool %q are unhealthy", health.UnhealthyThreshold, nodePoolName))...)
}

// ownsUnhealthyRepair returns true for a registered NodeClaim in a NodePool that never initialized and isn't rebooting.
func ownsUnhealthyRepair(nodeClaim *v1.NodeClaim) bool {
	conditions := nodeClaim.StatusConditions()
	return nodeClaim.Labels[v1.NodePoolLabelKey] != "" &&
		conditions.Get(v1.ConditionTypeRegistered).IsTrue() &&
		!conditions.Get(v1.ConditionTypeInitialized).IsTrue() &&
		!conditions.Get(v1.ConditionTypeRebooting).IsTrue()
}

// rebootFinishedAt returns when the last reboot finished, so unhealthy time from before or during it isn't counted.
func rebootFinishedAt(nodeClaim *v1.NodeClaim) time.Time {
	if rebooting := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting); rebooting != nil {
		return rebooting.LastTransitionTime.Time
	}
	return time.Time{}
}

// deleteUnhealthy marks the NodeClaim disrupted for repair and deletes it. The mark is persisted before the delete, so
// finalization can force the drain even after a restart, and evictions are attributed to repair. The mark is patched
// with an optimistic lock and the delete is preconditioned on the marked ResourceVersion, so neither happens if the
// NodeClaim changed, e.g. initialized, since it was read; the change requeues it.
func (l *Liveness) deleteUnhealthy(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node, result health.RepairResult) error {
	marked := nodeClaim.DeepCopy()
	marked.StatusConditions(status.WithClock(l.clock)).SetTrueWithReason(v1.ConditionTypeDisruptionReason,
		string(v1.DisruptionReasonUnhealthy), unhealthyRepairMessage)
	if err := l.kubeClient.Status().Patch(ctx, marked, client.MergeFromWithOptions(nodeClaim, client.MergeFromWithOptimisticLock{})); err != nil {
		if errors.IsConflict(err) {
			return nil
		}
		return client.IgnoreNotFound(serrors.Wrap(fmt.Errorf("marking nodeclaim disrupted, %w", err), "NodeClaim", klog.KObj(nodeClaim)))
	}
	if err := l.kubeClient.Delete(ctx, marked, client.Preconditions{ResourceVersion: lo.ToPtr(marked.ResourceVersion)}); err != nil {
		if errors.IsConflict(err) {
			return nil
		}
		return client.IgnoreNotFound(serrors.Wrap(fmt.Errorf("deleting nodeclaim, %w", err), "NodeClaim", klog.KObj(nodeClaim)))
	}
	// Carry the mark on the reconciled NodeClaim only once it's deleted, so the lifecycle status patch neither drops it nor,
	// after a failed delete, overwrites whatever changed the NodeClaim.
	nodeClaim.Status.Conditions = marked.Status.Conditions
	l.recorder.Publish(disruptionevents.Terminating(node, nodeClaim, string(v1.DisruptionReasonUnhealthy))...)
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
	reschedulablePods, err := nodeutils.ReschedulablePods(ctx, l.kubeClient, node.Name)
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
	return nil
}

// repairingUnhealthy returns true for a deleting NodeClaim that liveness is replacing as unhealthy, so its drain is forced.
func repairingUnhealthy(nodeClaim *v1.NodeClaim) bool {
	return markedForUnhealthyRepair(nodeClaim) && ownsUnhealthyRepair(nodeClaim)
}

// markedForUnhealthyRepair returns true when liveness marked the NodeClaim disrupted. The repair disruption method's mark
// shares the Unhealthy reason, including the one a repair reboot leaves until the node initializes again, so the message
// tells them apart.
func markedForUnhealthyRepair(nodeClaim *v1.NodeClaim) bool {
	reason := nodeClaim.StatusConditions().Get(v1.ConditionTypeDisruptionReason)
	return reason.IsTrue() && reason.Reason == string(v1.DisruptionReasonUnhealthy) && reason.Message == unhealthyRepairMessage
}
