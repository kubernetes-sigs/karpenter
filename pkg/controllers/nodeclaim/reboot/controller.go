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

// Package reboot implements the reboot node action: it drives a committed reboot (a NodeClaim carrying
// a Rebooting=True/RebootRequested condition) through drain -> issue -> observe to a terminal
// RebootSucceeded/RebootFailed outcome. A consumer (e.g. node repair) commits the reboot; this
// controller carries it out and hands back a terminal outcome. See designs/reboot-node-action.md.
package reboot

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	rebootevents "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/reboot/events"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

const (
	// Maximum time to observe a new boot and Ready node after reboot issuance.
	observationWindow = 20 * time.Minute
	// How often reboot recovery is re-checked.
	pollInterval = 15 * time.Second
	// Maximum time to retry provider acceptance after drain.
	issuanceTimeout = 5 * time.Minute
	// Minimum graceful drain window, including forceful reboots, to catch late pod bindings.
	// Mirrors the minimum-drain behavior from kubernetes-sigs/karpenter#2709.
	minDrainTime = 5 * time.Second
)

// Controller drives the reboot lifecycle for NodeClaims carrying an active Rebooting condition.
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	terminator    *terminator.Terminator
	recorder      events.Recorder

	// Tracks when each NodeClaim's provider-accept retry window started.
	// Process-local; a restart re-seeds the timeout in reconcileRequested.
	issuanceStartedMu sync.Mutex
	issuanceStarted   map[types.UID]time.Time
}

func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, t *terminator.Terminator, recorder events.Recorder) *Controller {
	return &Controller{
		clock:           clk,
		kubeClient:      kubeClient,
		cloudProvider:   cloudProvider,
		terminator:      t,
		recorder:        recorder,
		issuanceStarted: map[types.UID]time.Time{},
	}
}

func (c *Controller) Name() string {
	return "nodeclaim.reboot"
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.NodeClaim{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			nc, ok := o.(*v1.NodeClaim)
			return ok && nc.StatusConditions().Get(v1.ConditionTypeRebooting) != nil
		}))).
		WithOptions(controller.Options{RateLimiter: reasonable.RateLimiter(), MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting)
	// Only active reboots are reconciled here.
	if cond == nil || !cond.IsTrue() {
		return reconcile.Result{}, nil
	}
	// Deleting NodeClaims are handled by termination.
	if !nodeClaim.DeletionTimestamp.IsZero() {
		c.clearIssuanceStarted(nodeClaim.UID)
		return reconcile.Result{}, nil
	}

	node, err := nodeclaimutils.NodeForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		if !nodeclaimutils.IsNodeNotFoundError(err) {
			return reconcile.Result{}, err
		}
		// If the Node disappears mid-reboot, keep polling until the reboot deadline before failing. Without a Node
		// the drain can't run to start the issuance window, so bound that phase from the request instead.
		deadline, ok := c.rebootDeadline(nodeClaim)
		if !ok {
			deadline = rebootRequestedAt(nodeClaim).Add(issuanceTimeout)
		}
		if c.clock.Now().After(deadline) {
			result, msg := deadlineResult(nodeClaim)
			return c.transitionToFailed(ctx, nodeClaim, nil, result, msg)
		}
		return reconcile.Result{RequeueAfter: pollInterval}, nil
	}

	// Fail reboots that exceed their phase deadline instead of retrying indefinitely.
	if c.pastRebootDeadline(nodeClaim) {
		result, msg := deadlineResult(nodeClaim)
		return c.transitionToFailed(ctx, nodeClaim, node, result, msg)
	}

	switch cond.Reason {
	case v1.RebootReasonRequested:
		return c.reconcileRequested(ctx, nodeClaim, node)
	case v1.RebootReasonIssued:
		return c.reconcileIssued(ctx, nodeClaim, node)
	default:
		return reconcile.Result{}, nil
	}
}

// reconcileRequested applies the scheduling fence, drains, then issues the provider reboot.
func (c *Controller) reconcileRequested(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node) (reconcile.Result, error) {
	// Invalid or negative termination grace periods fail the reboot; absent means unbounded drain.
	tgp, err := rebootTerminationGracePeriod(nodeClaim)
	if err != nil {
		return c.transitionToFailed(ctx, nodeClaim, node, resultInvalidRequest, fmt.Sprintf("invalid reboot request: %v", err))
	}
	// Fence new scheduling before draining.
	if err := c.ensureRebootTaint(ctx, node); err != nil {
		return reconcile.Result{}, err
	}

	preBootID, issuing := nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey]
	if issuing {
		// If bootID already changed, the reboot happened; move to observation without re-issuing.
		if node.Status.NodeInfo.BootID != preBootID {
			return c.transitionToIssued(ctx, nodeClaim, node)
		}
		// Re-seed the process-local issuance timeout after restart.
		c.ensureIssuanceStarted(nodeClaim.UID)
	}

	// Drain only before the first provider call.
	if !issuing {
		if done, res, err := c.drain(ctx, nodeClaim, node, tgp); err != nil || !done {
			return res, err
		}
		// Record pre-boot state before the first provider call.
		if err := c.recordIssuingState(ctx, nodeClaim, node); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Use a stable operationID across retries and restarts.
	if err := c.cloudProvider.Reboot(ctx, nodeClaim, rebootOperationID(nodeClaim)); err != nil {
		if cloudprovider.IsNodeRebootNotImplementedError(err) {
			return c.transitionToFailed(ctx, nodeClaim, node, resultProviderError, "reboot not implemented by the cloud provider")
		}
		// Stay in RebootRequested and retry for transient provider errors.
		return reconcile.Result{}, fmt.Errorf("issuing reboot, %w", err)
	}
	return c.transitionToIssued(ctx, nodeClaim, node)
}

// reconcileIssued observes reboot recovery and completes once the new boot is Ready.
func (c *Controller) reconcileIssued(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node) (reconcile.Result, error) {
	// Re-apply the issued-state invariant after restart.
	if err := c.removeInitializedLabel(ctx, node); err != nil {
		return reconcile.Result{}, err
	}
	preBootID := nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey]
	bootChanged := node.Status.NodeInfo.BootID != preBootID

	// Remove the scheduling fence as soon as the new boot is observed.
	if bootChanged {
		if err := c.removeRebootTaint(ctx, node); err != nil {
			return reconcile.Result{}, err
		}
		c.recorder.Publish(rebootevents.RebootObserved(nodeClaim))
	}

	ready := nodeutils.GetCondition(node, corev1.NodeReady).Status == corev1.ConditionTrue
	if bootChanged && ready {
		return c.transitionToSucceeded(ctx, nodeClaim, node)
	}
	// Timeout is enforced by Reconcile.
	return reconcile.Result{RequeueAfter: pollInterval}, nil
}

// drain evicts pods before reboot, with residual pods allowed to ride the reboot post deadline.
// Every reboot observes at least minDrainTime to catch late pod bindings.
func (c *Controller) drain(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node, tgp *time.Duration) (done bool, res reconcile.Result, err error) {
	// Drain deadlines are measured from when the reboot was requested.
	floor := rebootRequestedAt(nodeClaim).Add(minDrainTime)
	var deadline *time.Time
	if tgp != nil {
		deadline = lo.ToPtr(rebootRequestedAt(nodeClaim).Add(max(*tgp, minDrainTime)))
	}
	if err := c.terminator.Drain(ctx, node, deadline); err != nil {
		if !terminator.IsNodeDrainError(err) {
			return false, reconcile.Result{}, fmt.Errorf("draining node, %w", err)
		}
		// Keep polling while pods are still draining.
		if deadline == nil {
			return false, reconcile.Result{RequeueAfter: pollInterval}, nil
		}
		if remaining := deadline.Sub(c.clock.Now()); remaining > 0 {
			return false, reconcile.Result{RequeueAfter: min(pollInterval, remaining)}, nil
		}
		return true, reconcile.Result{}, nil // deadline elapsed (always at or past the floor)
	}
	// Hold through the minimum drain window to catch late bindings.
	if remaining := floor.Sub(c.clock.Now()); remaining > 0 {
		return false, reconcile.Result{RequeueAfter: remaining}, nil
	}
	return true, reconcile.Result{}, nil
}

// recordIssuingState persists the pre-reboot bootID before the first provider call.
func (c *Controller) recordIssuingState(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node) error {
	// Start the process-local provider-accept timeout.
	c.ensureIssuanceStarted(nodeClaim.UID)
	// Persist bootID for restart-safe reboot detection and operationID generation.
	if nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey] == node.Status.NodeInfo.BootID {
		return nil
	}
	stored := nodeClaim.DeepCopy()
	nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
		v1.RebootPreBootIDAnnotationKey: node.Status.NodeInfo.BootID,
	})
	return c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored))
}

// ensureIssuanceStarted records the issuance start time once per process.
func (c *Controller) ensureIssuanceStarted(uid types.UID) {
	c.issuanceStartedMu.Lock()
	defer c.issuanceStartedMu.Unlock()
	if _, ok := c.issuanceStarted[uid]; !ok {
		c.issuanceStarted[uid] = c.clock.Now()
	}
}

func (c *Controller) clearIssuanceStarted(uid types.UID) {
	c.issuanceStartedMu.Lock()
	defer c.issuanceStartedMu.Unlock()
	delete(c.issuanceStarted, uid)
}

func (c *Controller) transitionToIssued(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node) (reconcile.Result, error) {
	stored := nodeClaim.DeepCopy()
	// Preserve the fault message while advancing to Issued.
	cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting)
	nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, cond.Message)
	// Initialization is boot-scoped; mark it Unknown while rebooting.
	nodeClaim.StatusConditions().SetUnknownWithReason(v1.ConditionTypeInitialized, v1.RebootReasonRebooting, "node is rebooting")
	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Best effort; reconcileIssued re-enforces this.
	if err := c.removeInitializedLabel(ctx, node); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: pollInterval}, nil
}

func (c *Controller) transitionToSucceeded(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node) (reconcile.Result, error) {
	if err := c.removeRebootTaint(ctx, node); err != nil {
		return reconcile.Result{}, err
	}
	// Capture durations before the terminal transition resets transition time.
	duration := c.clock.Since(rebootRequestedAt(nodeClaim))
	recovery, hasRecovery := time.Duration(0), false
	if issuedAt, ok := c.issuedAt(nodeClaim); ok {
		recovery, hasRecovery = c.clock.Since(issuedAt), true
	}
	if err := c.setTerminal(ctx, nodeClaim, v1.RebootReasonSucceeded, "node rebooted and rejoined the cluster"); err != nil {
		return reconcile.Result{}, err
	}
	// Record metrics only after the terminal state is persisted.
	recordTerminalMetrics(resultSucceeded, duration)
	if hasRecovery {
		// Recovery time is measured from issuance to rejoin.
		RebootRecoveryDurationSeconds.Observe(recovery.Seconds(), map[string]string{})
	}
	return reconcile.Result{}, nil
}

func (c *Controller) transitionToFailed(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node, result, msg string) (reconcile.Result, error) {
	// Remove the reboot fence when the Node still exists.
	if node != nil {
		if err := c.removeRebootTaint(ctx, node); err != nil {
			return reconcile.Result{}, err
		}
	}
	duration := c.clock.Since(rebootRequestedAt(nodeClaim))
	if result == resultInvalidRequest {
		// Invalid requests fail without replacing the node.
		if err := c.setTerminal(ctx, nodeClaim, v1.RebootReasonFailed, msg); err != nil {
			return reconcile.Result{}, err
		}
	} else {
		// Reboot failures after disruption escalate to NodeClaim replacement.
		if err := c.kubeClient.Delete(ctx, nodeClaim, client.Preconditions{ResourceVersion: lo.ToPtr(nodeClaim.ResourceVersion)}); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		c.clearIssuanceStarted(nodeClaim.UID)
		log.FromContext(ctx).WithValues("result", result).Info("reboot failed, replacing node")
	}
	// Record events and metrics only after the terminal write succeeds.
	c.recorder.Publish(rebootevents.RebootFailed(nodeClaim, msg))
	recordTerminalMetrics(result, duration)
	return reconcile.Result{}, nil
}

func (c *Controller) setTerminal(ctx context.Context, nodeClaim *v1.NodeClaim, reason, msg string) error {
	// Clear episode-scoped state so a later reboot starts clean.
	c.clearIssuanceStarted(nodeClaim.UID)
	// Use optimistic locking to avoid double-counting terminal metrics from stale reconciles.
	if _, hadPreBoot := nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey]; hadPreBoot {
		stored := nodeClaim.DeepCopy()
		delete(nodeClaim.Annotations, v1.RebootPreBootIDAnnotationKey)
		if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	stored := nodeClaim.DeepCopy()
	nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeRebooting, reason, msg)
	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	log.FromContext(ctx).WithValues("reason", reason).Info("reboot reached terminal outcome")
	return nil
}

func (c *Controller) ensureRebootTaint(ctx context.Context, node *corev1.Node) error {
	if _, found := lo.Find(node.Spec.Taints, func(t corev1.Taint) bool { return t.MatchTaint(&v1.RebootingNoScheduleTaint) }); found {
		return nil
	}
	stored := node.DeepCopy()
	node.Spec.Taints = append(node.Spec.Taints, v1.RebootingNoScheduleTaint)
	return c.kubeClient.Patch(ctx, node, client.MergeFrom(stored))
}

func (c *Controller) removeRebootTaint(ctx context.Context, node *corev1.Node) error {
	stored := node.DeepCopy()
	node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t corev1.Taint, _ int) bool { return t.MatchTaint(&v1.RebootingNoScheduleTaint) })
	if equality.Semantic.DeepEqual(stored, node) {
		return nil
	}
	return c.kubeClient.Patch(ctx, node, client.MergeFrom(stored))
}

func (c *Controller) removeInitializedLabel(ctx context.Context, node *corev1.Node) error {
	if _, ok := node.Labels[v1.NodeInitializedLabelKey]; !ok {
		return nil
	}
	stored := node.DeepCopy()
	delete(node.Labels, v1.NodeInitializedLabelKey)
	return c.kubeClient.Patch(ctx, node, client.MergeFrom(stored))
}

// Reads the reboot drain bound: absent = unbounded, 0 = forceful, >0 = graceful.
func rebootTerminationGracePeriod(nodeClaim *v1.NodeClaim) (*time.Duration, error) {
	value, ok := nodeClaim.Annotations[v1.RebootTerminationGracePeriodAnnotationKey]
	if !ok {
		return nil, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return nil, fmt.Errorf("parsing reboot termination grace period %q: %w", value, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("reboot termination grace period must be non-negative, got %q", value)
	}
	return &d, nil
}

// Derives reboot issuance time from Initialized transitioning to Unknown.
func (c *Controller) issuedAt(nodeClaim *v1.NodeClaim) (time.Time, bool) {
	cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
	if cond == nil || cond.Status != metav1.ConditionUnknown {
		return time.Time{}, false
	}
	return cond.LastTransitionTime.Time, true
}

// Returns the process-local start of the provider-accept retry window.
func (c *Controller) issuanceStartedAt(nodeClaim *v1.NodeClaim) (time.Time, bool) {
	c.issuanceStartedMu.Lock()
	defer c.issuanceStartedMu.Unlock()
	t, ok := c.issuanceStarted[nodeClaim.UID]
	return t, ok
}

// Returns the deadline for the current reboot phase, if bounded.
func (c *Controller) rebootDeadline(nodeClaim *v1.NodeClaim) (time.Time, bool) {
	if nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason == v1.RebootReasonIssued {
		// Recovery is bounded from the durable issuance transition.
		if issuedAt, ok := c.issuedAt(nodeClaim); ok {
			return issuedAt.Add(observationWindow), true
		}
		return time.Time{}, false
	}
	// Issuance is bounded from the process-local provider-accept start.
	if startedAt, ok := c.issuanceStartedAt(nodeClaim); ok {
		return startedAt.Add(issuanceTimeout), true
	}
	return time.Time{}, false
}

func (c *Controller) pastRebootDeadline(nodeClaim *v1.NodeClaim) bool {
	deadline, ok := c.rebootDeadline(nodeClaim)
	return ok && c.clock.Now().After(deadline)
}

// Maps a phase timeout to its terminal result.
func deadlineResult(nodeClaim *v1.NodeClaim) (result, msg string) {
	if nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason == v1.RebootReasonIssued {
		return resultRecoveryTimeout, "node did not recover within the observation window"
	}
	return resultProviderError, "reboot was not issued within the issuance timeout"
}

// Returns when the current reboot episode was requested.
func rebootRequestedAt(nodeClaim *v1.NodeClaim) time.Time {
	return nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).LastTransitionTime.Time
}

// Returns a stable per-episode idempotency key for provider reboot calls.
func rebootOperationID(nodeClaim *v1.NodeClaim) string {
	return fmt.Sprintf("%s-%d-%s", nodeClaim.UID, rebootRequestedAt(nodeClaim).UnixNano(), nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey])
}

// Records the terminal result and total reboot duration.
func recordTerminalMetrics(result string, duration time.Duration) {
	RebootsTotal.Inc(map[string]string{resultLabel: result})
	RebootDurationSeconds.Observe(duration.Seconds(), map[string]string{resultLabel: result})
}
