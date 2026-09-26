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
	// observationWindow bounds how long we wait for a rebooted node to prove a new boot and rejoin
	// after issuance before declaring RebootFailed. Beta uses a fixed value sized for slow instances.
	observationWindow = 20 * time.Minute
	// pollInterval is how often we re-check for boot/readiness while observing recovery.
	pollInterval = 15 * time.Second
	// issuanceTimeout bounds the post-drain provider-accept retry loop before declaring RebootFailed. Mirrors
	// nodeclaim lifecycle's LaunchTimeout. The issuance start is process-local (see Controller.issuanceStarted)
	// and re-seeded on restart, so this bounds a window of continuous uptime, not a single durable deadline.
	issuanceTimeout = 5 * time.Minute
	// minDrainTime is the floor on the graceful drain window, applied even to a forceful (0s) reboot: a pod
	// can bind to the node after the fence taint is applied but before scheduler informers catch up, so we
	// make at least one graceful eviction pass over this window rather than letting it ride the reboot.
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

	// issuanceStartedMu guards issuanceStarted, the process-local record of when each episode's post-drain
	// provider-accept loop began (keyed by NodeClaim UID). It bounds the issuance retry loop and is
	// deliberately not persisted (avoids another annotation); a restart re-seeds it in reconcileRequested, so
	// a restart restarts the issuance-timeout window rather than dropping the bound. See rebootDeadline.
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
	// Only active (True) reboots are driven here. Absent or terminal (False) => nothing to do.
	if cond == nil || !cond.IsTrue() {
		return reconcile.Result{}, nil
	}

	node, err := nodeclaimutils.NodeForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		if !nodeclaimutils.IsNodeNotFoundError(err) {
			return reconcile.Result{}, err
		}
		// The Node is gone mid-reboot: we can neither observe recovery nor clean the fence (it went with
		// the Node). Fail if the deadline has elapsed, otherwise keep polling until it does — never a
		// silent no-requeue stop, which would wedge the NodeClaim at Rebooting=True forever.
		if c.pastRebootDeadline(nodeClaim) {
			result, msg := deadlineResult(nodeClaim)
			return c.transitionToFailed(ctx, nodeClaim, nil, result, msg)
		}
		return reconcile.Result{RequeueAfter: pollInterval}, nil
	}

	// Bound every phase: a reboot that never issues (request phase) or never recovers (observe phase) is
	// failed here rather than retrying forever, since Rebooting=True excludes the node from other
	// disruption and advertises returning capacity to the scheduler.
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

// reconcileRequested applies the scheduling fence, drains (bounded), then issues the provider reboot.
func (c *Controller) reconcileRequested(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node) (reconcile.Result, error) {
	// A committed reboot request must carry a valid reboot termination grace period; reject an invalid one
	// terminally rather than defaulting to a (possibly destructive) forceful reboot.
	tgp, err := rebootTerminationGracePeriod(nodeClaim)
	if err != nil {
		return c.transitionToFailed(ctx, nodeClaim, node, resultInvalidRequest, fmt.Sprintf("invalid reboot request: %v", err))
	}
	// Scheduling fence: reboot-owned taint that keeps evicted pods from rescheduling onto the pre-reboot boot.
	if err := c.ensureRebootTaint(ctx, node); err != nil {
		return reconcile.Result{}, err
	}

	// Restart-safety: once we've begun issuing (pre-boot bootID recorded), a changed bootID proves the
	// reboot already happened — advance to observe without re-issuing.
	preBootID, issuing := nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey]
	if issuing {
		// A changed bootID proves the reboot already happened — advance to observe without re-issuing.
		if node.Status.NodeInfo.BootID != preBootID {
			return c.transitionToIssued(ctx, nodeClaim, node)
		}
		// Resuming the issuance loop (e.g. after a controller restart): the issuance-start timestamp is
		// process-local, so re-seed it when missing. This restarts the issuance-timeout window rather than
		// dropping the bound — evading the timeout requires the controller to restart within every window.
		c.ensureIssuanceStarted(nodeClaim.UID)
	}

	// Drain before issuing (only before we've recorded pre-boot state; on resume after that, skip drain).
	if !issuing {
		if done, res, err := c.drain(ctx, nodeClaim, node, tgp); err != nil || !done {
			return res, err
		}
		// Record pre-boot state before the first provider call.
		if err := c.recordIssuingState(ctx, nodeClaim, node); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Issue the reboot with a deterministic, per-episode operationID (stable across retries/restarts).
	if err := c.cloudProvider.Reboot(ctx, nodeClaim, rebootOperationID(nodeClaim)); err != nil {
		if cloudprovider.IsNodeRebootNotImplementedError(err) {
			return c.transitionToFailed(ctx, nodeClaim, node, resultProviderError, "reboot not implemented by the cloud provider")
		}
		// Transient error: stay in RebootRequested and retry with backoff using the same operationID.
		return reconcile.Result{}, fmt.Errorf("issuing reboot, %w", err)
	}
	return c.transitionToIssued(ctx, nodeClaim, node)
}

// reconcileIssued observes recovery: remove the fence once the boot changes, succeed on a fresh boot +
// Ready, fail if the observation window elapses first.
func (c *Controller) reconcileIssued(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node) (reconcile.Result, error) {
	// Re-enforce the RebootIssued invariant (idempotent) so a crash after the transition can't strand the label.
	if err := c.removeInitializedLabel(ctx, node); err != nil {
		return reconcile.Result{}, err
	}
	preBootID := nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey]
	bootChanged := node.Status.NodeInfo.BootID != preBootID

	// The fence exists only while the pre-reboot boot may still be active. A changed bootID proves the
	// new boot has begun, so remove it immediately (independent of readiness).
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
	// The observation-window timeout is enforced by the phase-agnostic deadline check in Reconcile.
	return reconcile.Result{RequeueAfter: pollInterval}, nil
}

// drain runs a bounded graceful drain (eviction only, no cordon). Returns done=true when the drain completes
// or the deadline elapses (residual pods ride the reboot). The window is max(reboot termination grace period,
// minDrainTime), so even a forceful (0s) reboot makes a graceful eviction pass; a node with no pods to evict
// returns done immediately, so the floor only delays reboots that actually have workloads to drain.
func (c *Controller) drain(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node, tgp time.Duration) (done bool, res reconcile.Result, err error) {
	// Deadline is measured from when the reboot was requested (the Rebooting condition's transition).
	deadline := rebootRequestedAt(nodeClaim).Add(max(tgp, minDrainTime))
	if err := c.terminator.Drain(ctx, node, &deadline); err != nil {
		if !terminator.IsNodeDrainError(err) {
			return false, reconcile.Result{}, fmt.Errorf("draining node, %w", err)
		}
		// Pods still draining: re-check every pollInterval (or sooner as the deadline nears) so we advance as
		// soon as the node is empty, then proceed with residual pods riding once the deadline elapses.
		if remaining := deadline.Sub(c.clock.Now()); remaining > 0 {
			return false, reconcile.Result{RequeueAfter: min(pollInterval, remaining)}, nil
		}
	}
	return true, reconcile.Result{}, nil
}

// recordIssuingState records the pre-reboot bootID before the first provider call, so a changed bootID
// afterward proves the reboot happened (restart-safety) and terminal cleanup can scope to this episode.
func (c *Controller) recordIssuingState(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node) error {
	// Anchor the post-drain issuance timeout in memory (drain-completion time). It only bounds the
	// provider-accept retry loop, so it isn't persisted; a restart re-seeds it in reconcileRequested.
	c.ensureIssuanceStarted(nodeClaim.UID)
	// The pre-reboot bootID must be durable: restart-safety and the operationID both derive from it, so a
	// changed bootID after a restart still proves the reboot happened. Persist it before the provider call.
	if nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey] == node.Status.NodeInfo.BootID {
		return nil
	}
	stored := nodeClaim.DeepCopy()
	nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
		v1.RebootPreBootIDAnnotationKey: node.Status.NodeInfo.BootID,
	})
	return c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored))
}

// ensureIssuanceStarted records the issuance-start time for this episode if not already set (set-if-absent),
// so the first drain-completion or a post-restart resume seeds it and repeated calls are no-ops.
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
	// Carry the driving-fault message forward across the phase change; the reason marks the phase (Issued).
	cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting)
	nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, cond.Message)
	// Initialization is scoped to a boot; the Initialized->Unknown transition also stamps issuedAt. It's set
	// at issue time, so the reason is Rebooting (the reboot is in flight), not RebootRequested.
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
	// Capture durations before setTerminal resets the Rebooting condition's transition time.
	duration := c.clock.Since(rebootRequestedAt(nodeClaim))
	recovery, hasRecovery := time.Duration(0), false
	if issuedAt, ok := c.issuedAt(nodeClaim); ok {
		recovery, hasRecovery = c.clock.Since(issuedAt), true
	}
	if err := c.setTerminal(ctx, nodeClaim, v1.RebootReasonSucceeded, "node rebooted and rejoined the cluster"); err != nil {
		return reconcile.Result{}, err
	}
	// Record metrics only after the terminal patch is durable, so a patch-conflict requeue can't re-enter
	// this branch and double-count.
	recordTerminalMetrics(resultSucceeded, duration)
	if hasRecovery {
		// Recovery duration (drain-independent): issuance -> new boot rejoined. Success only — a timed-out
		// reboot never recovered, so it has no recovery time (this is why it's not observed on failure).
		RebootRecoveryDurationSeconds.Observe(recovery.Seconds(), map[string]string{})
	}
	return reconcile.Result{}, nil
}

func (c *Controller) transitionToFailed(ctx context.Context, nodeClaim *v1.NodeClaim, node *corev1.Node, result, msg string) (reconcile.Result, error) {
	// Terminal cleanup: ensure the reboot-owned fence is removed even if the boot never changed. node may
	// be nil when it was deleted mid-reboot, in which case the fence went with it — nothing to clean.
	if node != nil {
		if err := c.removeRebootTaint(ctx, node); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Capture duration before setTerminal resets the Rebooting condition's transition time.
	duration := c.clock.Since(rebootRequestedAt(nodeClaim))
	if err := c.setTerminal(ctx, nodeClaim, v1.RebootReasonFailed, msg); err != nil {
		return reconcile.Result{}, err
	}
	// Record events/metrics only after the terminal patch is durable (see transitionToSucceeded).
	c.recorder.Publish(rebootevents.RebootFailed(nodeClaim, msg))
	recordTerminalMetrics(result, duration)
	// Escalate to replacement: a reboot that reached the drain step already disrupted the node (its
	// workloads were drained/fenced and it is NotReady/uninitialized), so it can't be cleanly returned to
	// service. Delete the NodeClaim — the termination finalizer drains + terminates and provisioning
	// replaces it. The sole exception is a pre-drain invalid_request (the node is untouched; it's a
	// producer-contract violation), which we surface without destroying the node.
	if result != resultInvalidRequest {
		if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) setTerminal(ctx context.Context, nodeClaim *v1.NodeClaim, reason, msg string) error {
	// Clear episode-scoped reboot state first, so a later reboot on this NodeClaim starts clean and the
	// restart-safety check can't misfire on a prior episode's pre-boot bootID. The issuance start is
	// in-memory (see recordIssuingState); the pre-boot bootID is the only persisted episode annotation.
	c.clearIssuanceStarted(nodeClaim.UID)
	if _, hadPreBoot := nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey]; hadPreBoot {
		stored := nodeClaim.DeepCopy()
		delete(nodeClaim.Annotations, v1.RebootPreBootIDAnnotationKey)
		if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
			return err
		}
	}
	stored := nodeClaim.DeepCopy()
	nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeRebooting, reason, msg)
	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
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

// rebootTerminationGracePeriod reads the consumer-stamped reboot termination grace period (the drain bound).
// A committed reboot request must carry a valid, non-negative value: 0s = forceful, >0 = graceful-bounded.
// Missing, malformed, or negative is a producer contract violation (0s already means forceful), so it's an
// error the caller fails terminally rather than silently selecting the most disruptive behavior.
func rebootTerminationGracePeriod(nodeClaim *v1.NodeClaim) (time.Duration, error) {
	value, ok := nodeClaim.Annotations[v1.RebootTerminationGracePeriodAnnotationKey]
	if !ok {
		return 0, fmt.Errorf("reboot termination grace period annotation is missing")
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parsing reboot termination grace period %q: %w", value, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("reboot termination grace period must be non-negative, got %q", value)
	}
	return d, nil
}

// issuedAt derives the issuance time from the Initialized condition, which transitions to Unknown exactly
// when the reboot is issued and is held there (by the initialization guard) until the reboot is terminal.
func (c *Controller) issuedAt(nodeClaim *v1.NodeClaim) (time.Time, bool) {
	cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
	if cond == nil || cond.Status != metav1.ConditionUnknown {
		return time.Time{}, false
	}
	return cond.LastTransitionTime.Time, true
}

// issuanceStartedAt returns when this episode's post-drain provider-accept loop began, if the controller
// recorded it in memory during this process lifetime. It's absent before the drain completes and after a
// controller restart; in the latter case the issuance timeout simply does not apply (see rebootDeadline).
func (c *Controller) issuanceStartedAt(nodeClaim *v1.NodeClaim) (time.Time, bool) {
	c.issuanceStartedMu.Lock()
	defer c.issuanceStartedMu.Unlock()
	t, ok := c.issuanceStarted[nodeClaim.UID]
	return t, ok
}

// rebootDeadline is the wall-clock bound for the current phase, after which the reboot is failed. It is
// derived only from the NodeClaim (never the Node), so it fires even when the Node has been deleted. The
// bool is false when the current phase has no bound, in which case the reboot keeps polling rather than
// failing.
func (c *Controller) rebootDeadline(nodeClaim *v1.NodeClaim) (time.Time, bool) {
	if nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason == v1.RebootReasonIssued {
		// Observation window, anchored on the durable Initialized->Unknown transition (see issuedAt), so it
		// bounds recovery even across controller restarts.
		if issuedAt, ok := c.issuedAt(nodeClaim); ok {
			return issuedAt.Add(observationWindow), true
		}
		return time.Time{}, false
	}
	// RebootRequested: bound the post-drain provider-accept loop from the process-local issuance start. Before
	// the drain completes (or on the first reconcile after a restart, before reconcileRequested re-seeds the
	// start) there's no bound for that pass; drain() bounds itself, and the re-seed restarts the window on the
	// next pass. Evading the timeout therefore requires the controller to restart within every window.
	if startedAt, ok := c.issuanceStartedAt(nodeClaim); ok {
		return startedAt.Add(issuanceTimeout), true
	}
	return time.Time{}, false
}

func (c *Controller) pastRebootDeadline(nodeClaim *v1.NodeClaim) bool {
	deadline, ok := c.rebootDeadline(nodeClaim)
	return ok && c.clock.Now().After(deadline)
}

// deadlineResult maps the current phase to the terminal result label and message used when the deadline
// elapses: the request phase failed to issue (provider_error); the observe phase failed to recover.
func deadlineResult(nodeClaim *v1.NodeClaim) (result, msg string) {
	if nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason == v1.RebootReasonIssued {
		return resultRecoveryTimeout, "node did not recover within the observation window"
	}
	return resultProviderError, "reboot was not issued within the issuance timeout"
}

// rebootRequestedAt is when the current reboot episode was committed: the Rebooting condition's transition
// to True. operatorpkg preserves LastTransitionTime across the RebootRequested->RebootIssued reason change
// (status stays True), so this is stable for the whole episode until the terminal SetFalse.
func rebootRequestedAt(nodeClaim *v1.NodeClaim) time.Time {
	return nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).LastTransitionTime.Time
}

// rebootOperationID is a deterministic, per-episode idempotency key passed to CloudProvider.Reboot: stable
// across retries and controller restarts within an episode, and distinct across episodes. Derived from the
// request time and the pre-reboot bootID (recorded before issuing) rather than stored, so no annotation is
// needed and stale keys can't leak. The bootID disambiguates episodes within the same second, since
// metav1.Time (the request time's source) only round-trips at second precision.
func rebootOperationID(nodeClaim *v1.NodeClaim) string {
	return fmt.Sprintf("%s-%d-%s", nodeClaim.UID, rebootRequestedAt(nodeClaim).UnixNano(), nodeClaim.Annotations[v1.RebootPreBootIDAnnotationKey])
}

// recordTerminalMetrics counts the reboot by result and observes the full-action duration (request ->
// terminal). Called only after the terminal condition patch succeeds, so a patch-conflict requeue cannot
// re-enter the terminal branch and double-count.
func recordTerminalMetrics(result string, duration time.Duration) {
	RebootsTotal.Inc(map[string]string{resultLabel: result})
	RebootDurationSeconds.Observe(duration.Seconds(), map[string]string{resultLabel: result})
}
