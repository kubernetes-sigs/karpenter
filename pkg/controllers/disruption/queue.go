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

package disruption

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	operatorlogging "sigs.k8s.io/karpenter/pkg/operator/logging"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	utilscontroller "sigs.k8s.io/karpenter/pkg/utils/controller"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

const (
	queueBaseDelay          = 1 * time.Second
	queueMaxDelay           = 10 * time.Second
	minRetryDuration        = 10 * time.Minute
	maxRetryDuration        = 1 * time.Hour
	maxConcurrentReconciles = 100
	retryDurationScale      = 80 * time.Millisecond
)

type UnrecoverableError struct {
	error
}

func NewUnrecoverableError(err error) *UnrecoverableError {
	return &UnrecoverableError{error: err}
}

func IsUnrecoverableError(err error) bool {
	if err == nil {
		return false
	}
	var unrecoverableError *UnrecoverableError
	return stderrors.As(err, &unrecoverableError)
}

func (q *Queue) GetMaxRetryDuration() time.Duration {
	q.RLock()
	numCommands := len(q.ProviderIDToCommand)
	q.RUnlock()
	retryDuration := retryDurationScale * time.Duration(numCommands)
	return lo.Clamp(retryDuration, minRetryDuration, maxRetryDuration)
}

type Queue struct {
	sync.RWMutex
	ProviderIDToCommand map[string]*Command // providerID -> command, maps a candidate to its command
	source              chan event.TypedGenericEvent[*v1.NodeClaim]
	kubeClient          client.Client
	apiReader           client.Reader
	recorder            events.Recorder
	cluster             *state.Cluster
	clock               clock.Clock
	provisioner         *provisioning.Provisioner
}

// NewQueue creates a queue that will asynchronously orchestrate disruption commands
func NewQueue(kubeClient client.Client, recorder events.Recorder, cluster *state.Cluster, clock clock.Clock,
	provisioner *provisioning.Provisioner, apiReader client.Reader,
) *Queue {
	queue := &Queue{
		// nolint:staticcheck
		// We need to implement a deprecated interface since Command currently doesn't implement "comparable"
		source:              make(chan event.TypedGenericEvent[*v1.NodeClaim], 10000),
		ProviderIDToCommand: map[string]*Command{},
		kubeClient:          kubeClient,
		apiReader:           apiReader,
		recorder:            recorder,
		cluster:             cluster,
		clock:               clock,
		provisioner:         provisioner,
	}
	return queue
}

func (q *Queue) Name() string {
	return "disruption.queue"
}

func (q *Queue) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(q.Name()).
		WatchesRawSource(source.Channel(q.source, &handler.TypedEnqueueRequestForObject[*v1.NodeClaim]{})).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
				workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](queueBaseDelay, queueMaxDelay),
				&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(100), 1000)},
			),
			MaxConcurrentReconciles: utilscontroller.LinearScaleReconciles(utilscontroller.CPUCount(ctx), 100, 1000),
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), q))
}

func (q *Queue) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, q.Name())
	q.RLock()
	cmd, exists := q.ProviderIDToCommand[nodeClaim.Status.ProviderID]
	q.RUnlock()
	if !exists {
		log.FromContext(ctx).Error(fmt.Errorf("no command found"), "")
		return reconcile.Result{}, nil
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues(cmd.LogValues()...))

	if err := q.waitOrTerminate(ctx, cmd); err != nil {
		// If recoverable, re-queue and try again.
		if !IsUnrecoverableError(err) {
			return reconcile.Result{RequeueAfter: queueBaseDelay}, nil
		}
		// If the command failed, bail on the action.
		// 1. Emit metrics for launch failures
		// 2. Ensure cluster state no longer thinks these nodes are deleting
		// 3. Remove it from the Queue's internal data structure
		failedLaunches := lo.Filter(cmd.Replacements, func(r *Replacement, _ int) bool {
			return !r.Initialized
		})
		DisruptionQueueFailuresTotal.Add(float64(len(failedLaunches)), map[string]string{
			decisionLabel:          string(cmd.Decision()),
			metrics.ReasonLabel:    pretty.ToSnakeCase(string(cmd.Reason())),
			ConsolidationTypeLabel: string(cmd.Decision()),
		})
		multiErr := multierr.Combine(err, q.rollbackCandidates(ctx, cmd.Candidates, cmd.taintedCandidates), q.cleanupUnusedReplacements(ctx, cmd))
		// Log the error
		log.FromContext(ctx).Error(multiErr, "failed terminating nodes while executing a disruption command")
	} else {
		log.FromContext(ctx).V(1).Info("command succeeded")
		cmd.Succeeded = true
	}
	q.CompleteCommand(cmd)
	return reconcile.Result{}, nil
}

// waitOrTerminate will wait until launched nodeclaims are ready.
// Once the replacements are ready, it will terminate the candidates.
// nolint:gocyclo
func (q *Queue) waitOrTerminate(ctx context.Context, cmd *Command) (err error) {
	// We use the number of commands in the queue as a proxy for cloud provider traffic.
	// As the number of commands increase, we expect more delays and scale the retry duration accordingly.
	retryDuration := q.GetMaxRetryDuration()
	// Wrap an error in an unrecoverable error if it timed out
	defer func() {
		if q.clock.Since(cmd.CreationTimestamp) > retryDuration {
			err = NewUnrecoverableError(serrors.Wrap(fmt.Errorf("command reached timeout, %w", err), "duration", q.clock.Since(cmd.CreationTimestamp)))
		}
	}()
	waitErrs := make([]error, len(cmd.Replacements))
	for i := range cmd.Replacements {
		// If we know the node claim is Initialized, no need to check again.
		if cmd.Replacements[i].Initialized {
			continue
		}
		// Get the nodeclaim
		nodeClaim := &v1.NodeClaim{}
		if err := q.kubeClient.Get(ctx, types.NamespacedName{Name: cmd.Replacements[i].Name}, nodeClaim); err != nil {
			// The NodeClaim got deleted after an initial eventual consistency delay
			// This means that there was an ICE error or the Node initializationTTL expired
			// In this case, the error is unrecoverable, so don't requeue.
			if errors.IsNotFound(err) && !q.cluster.NodeClaimExists(cmd.Replacements[i].Name) {
				return NewUnrecoverableError(fmt.Errorf("replacement was deleted, %w", err))
			}
			waitErrs[i] = fmt.Errorf("getting node claim, %w", err)
			continue
		}
		// We emitted this event when disruption was blocked on launching/termination.
		// This does not block other forms of deprovisioning, but we should still emit this.
		q.recorder.Publish(disruptionevents.Launching(nodeClaim, string(cmd.Reason())))
		initializedStatus := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
		if !initializedStatus.IsTrue() {
			q.recorder.Publish(disruptionevents.WaitingOnReadiness(nodeClaim))
			waitErrs[i] = serrors.Wrap(fmt.Errorf("nodeclaim not initialized"), "NodeClaim", klog.KRef("", nodeClaim.Name))
			continue
		}
		cmd.Replacements[i].Initialized = true
	}
	// If we have any errors, don't continue
	if err := multierr.Combine(waitErrs...); err != nil {
		return fmt.Errorf("waiting for replacement initialization, %w", err)
	}

	// A bounded multi-node multi-replacement command deletes more sources than it launches replacements, so
	// initialization alone isn't a strong enough barrier. Every other disruption command keeps its existing
	// behavior.
	if isMultiNodeMultiReplacementCommand(ctx, cmd) {
		if err := q.ensureReplacementsReady(ctx, cmd); err != nil {
			return err
		}
	}

	// All replacements have been provisioned.
	// All we need to do now is get a successful delete call for each node claim,
	// then the termination controller will handle the eventual deletion of the nodes.
	errs := make([]error, len(cmd.Candidates))
	workqueue.ParallelizeUntil(ctx, len(cmd.Candidates), len(cmd.Candidates), func(i int) {
		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return client.IgnoreNotFound(err) != nil }, func() error {
			return q.kubeClient.Delete(ctx, cmd.Candidates[i].NodeClaim)
		}); err != nil {
			errs[i] = client.IgnoreNotFound(err)
			return
		}
		q.recorder.Publish(disruptionevents.Terminating(cmd.Candidates[i].Node, cmd.Candidates[i].NodeClaim, string(cmd.Reason()))...)
		// Drift also flows through this queue, so only report a policy for consolidation.
		consolidationPolicy := ""
		if cmd.ConsolidationType() != "" {
			consolidationPolicy = pretty.ToSnakeCase(string(cmd.Candidates[i].NodePool.Spec.Disruption.ConsolidationPolicy))
		}
		labels := map[string]string{
			metrics.ReasonLabel:              pretty.ToSnakeCase(string(cmd.Reason())),
			metrics.NodePoolLabel:            cmd.Candidates[i].NodeClaim.Labels[v1.NodePoolLabelKey],
			metrics.CapacityTypeLabel:        cmd.Candidates[i].NodeClaim.Labels[v1.CapacityTypeLabelKey],
			metrics.ConsolidationPolicyLabel: consolidationPolicy,
			metrics.TerminationModeLabel:     nodeclaimutils.DisruptionTerminationMode(cmd.Candidates[i].NodeClaim),
		}
		metrics.NodeClaimsDisruptedTotal.Inc(labels)
		metrics.PodsDisruptionInitiatedTotal.Add(float64(len(cmd.Candidates[i].reschedulablePods)), labels)
	})
	// If there were any deletion failures, we should requeue.
	// In the case where we requeue, but the timeout for the command is reached, we'll mark this as a failure.
	return multierr.Combine(errs...)
}

// markDisrupted taints the node and adds the Disrupted condition to the NodeClaim for a candidate that is about to be disrupted
// For static NodeClaims, we mark NodeClaims as pendingdisruption in statenodepool
// It returns the fully marked candidates as well as every candidate this command tainted, so that a later failure
// only rolls back candidates that this command actually touched.
func (q *Queue) markDisrupted(ctx context.Context, cmd *Command) ([]*Candidate, []*Candidate, error) {
	errs := make([]error, len(cmd.Candidates))
	tainted := make([]bool, len(cmd.Candidates))
	workqueue.ParallelizeUntil(ctx, len(cmd.Candidates), len(cmd.Candidates), func(i int) {
		alreadyTainted := false
		if cmd.Candidates[i].Node != nil {
			node := &corev1.Node{}
			if err := q.apiReader.Get(ctx, client.ObjectKey{Name: cmd.Candidates[i].Node.Name}, node); err != nil {
				errs[i] = fmt.Errorf("getting node before tainting, %w", err)
				return
			}
			alreadyTainted = lo.Contains(node.Spec.Taints, v1.DisruptedNoScheduleTaint)
		}
		if err := state.RequireNoScheduleTaint(ctx, q.kubeClient, true, cmd.Candidates[i].StateNode); err != nil {
			errs[i] = serrors.Wrap(fmt.Errorf("tainting nodes, %w", err), "taint", pretty.Taint(v1.DisruptedNoScheduleTaint))
			return
		}
		tainted[i] = !alreadyTainted
		// refresh nodeclaim before updating status
		nodeClaim := &v1.NodeClaim{}
		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return client.IgnoreNotFound(err) != nil }, func() error {
			if e := q.kubeClient.Get(ctx, client.ObjectKeyFromObject(cmd.Candidates[i].NodeClaim), nodeClaim); e != nil {
				return e
			}
			stored := nodeClaim.DeepCopy()
			nodeClaim.StatusConditions(status.WithClock(q.clock)).SetTrueWithReason(v1.ConditionTypeDisruptionReason, string(cmd.Reason()), string(cmd.Reason()))
			return q.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored))
		}); err != nil {
			errs[i] = client.IgnoreNotFound(err)
			return
		}
	})
	var markedCandidates, taintedCandidates []*Candidate
	for i := range errs {
		if tainted[i] {
			taintedCandidates = append(taintedCandidates, cmd.Candidates[i])
		}
		if errs[i] != nil {
			continue
		}
		markedCandidates = append(markedCandidates, cmd.Candidates[i])

		// Mark all StaticNodeClaims as pendingdisruption in nodepoolstate
		if cmd.Candidates[i].OwnedByStaticNodePool() {
			q.cluster.NodePoolState.MarkNodeClaimPendingDisruption(cmd.Candidates[i].NodePool.Name, cmd.Candidates[i].NodeClaim.Name)
		}
	}
	return markedCandidates, taintedCandidates, multierr.Combine(errs...)
}

// createReplacementNodeClaims creates replacement NodeClaims
func (q *Queue) createReplacementNodeClaims(ctx context.Context, cmd *Command) error {
	nodeClaimNames, err := q.provisioner.CreateNodeClaims(ctx, lo.Map(cmd.Replacements, func(r *Replacement, _ int) *pscheduling.NodeClaim { return r.NodeClaim }), provisioning.WithReason(strings.ToLower(string(cmd.Reason()))))
	// Record the names that were created, even on a partial failure, so that cleanup can find them.
	for i, name := range nodeClaimNames {
		if i < len(cmd.Replacements) {
			cmd.Replacements[i].Name = name
		}
	}
	if err != nil {
		return err
	}
	if len(nodeClaimNames) != len(cmd.Replacements) {
		// shouldn't ever occur since a partially failed CreateNodeClaims should return an error
		return serrors.Wrap(fmt.Errorf("expected replacement count did not equal actual replacement count"), "expected-count", len(cmd.Replacements), "actual-count", len(nodeClaimNames))
	}
	return nil
}

func (q *Queue) rollbackCandidates(ctx context.Context, markedCandidates, taintedCandidates []*Candidate) error {
	q.cluster.UnmarkForDeletion(lo.Map(markedCandidates, func(candidate *Candidate, _ int) string { return candidate.ProviderID() })...)
	for _, candidate := range markedCandidates {
		if candidate.OwnedByStaticNodePool() {
			q.cluster.NodePoolState.MarkNodeClaimActive(candidate.NodePool.Name, candidate.NodeClaim.Name)
		}
	}
	return multierr.Combine(
		state.RequireNoScheduleTaint(ctx, q.kubeClient, false,
			lo.Map(taintedCandidates, func(candidate *Candidate, _ int) *state.StateNode { return candidate.StateNode })...),
		state.ClearNodeClaimsCondition(ctx, q.kubeClient, q.clock, v1.ConditionTypeDisruptionReason,
			lo.Map(markedCandidates, func(candidate *Candidate, _ int) *state.StateNode { return candidate.StateNode })...),
	)
}

// cleanupUnusedReplacements deletes replacements that a failed bounded multi-node multi-replacement command
// launched but never used. It is scoped to that command shape so that every other disruption command keeps its
// existing behavior of leaving replacements in place.
func (q *Queue) cleanupUnusedReplacements(ctx context.Context, cmd *Command) error {
	if !isMultiNodeMultiReplacementCommand(ctx, cmd) {
		return nil
	}
	errs := make([]error, len(cmd.Replacements))
	for i, replacement := range cmd.Replacements {
		if replacement.Name == "" || replacement.Initialized {
			continue
		}
		// Read the current NodeClaim instead of relying on the creation-time object. A creation-time
		// resourceVersion goes stale as soon as the NodeClaim's launch status is written, which made the delete
		// fail with a conflict that was then silently ignored.
		nodeClaim := &v1.NodeClaim{}
		if err := q.apiReader.Get(ctx, types.NamespacedName{Name: replacement.Name}, nodeClaim); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			errs[i] = serrors.Wrap(fmt.Errorf("getting unused replacement, %w", err), "NodeClaim", klog.KRef("", replacement.Name))
			continue
		}
		if reason := replacementRetentionReason(nodeClaim); reason != "" {
			log.FromContext(ctx).WithValues("NodeClaim", klog.KObj(nodeClaim), "reason", reason).V(1).Info("retaining replacement after failed disruption command")
			continue
		}
		// The UID precondition binds the delete to the NodeClaim that was just read, so a NodeClaim that is
		// concurrently replaced is never deleted by this cleanup.
		uid := nodeClaim.UID
		if err := q.kubeClient.Delete(ctx, nodeClaim, client.Preconditions{UID: &uid}); err != nil {
			errs[i] = client.IgnoreNotFound(serrors.Wrap(fmt.Errorf("deleting unused replacement, %w", err), "NodeClaim", klog.KObj(nodeClaim)))
		}
	}
	return multierr.Combine(errs...)
}

// replacementRetentionReason returns a non-empty reason when an unused replacement must be retained instead of
// deleted, because it already progressed far enough that it may be registering or carrying workloads.
func replacementRetentionReason(nodeClaim *v1.NodeClaim) string {
	switch {
	case !nodeClaim.DeletionTimestamp.IsZero():
		return "terminating"
	case nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue():
		return "initialized"
	case nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue():
		return "registered"
	case nodeClaim.Status.NodeName != "":
		return "backing-node-exists"
	}
	return ""
}

// ensureReplacementsReady re-verifies that every replacement still exists, is still initialized, and has a Ready
// backing Node. Initialization is sticky for metrics, but deleting three sources for two replacements requires the
// current state of both replacements to hold at the final barrier. Missing node state is retried rather than
// treated as unrecoverable, since the informer can lag behind NodeClaim initialization.
//
//nolint:gocyclo
func (q *Queue) ensureReplacementsReady(ctx context.Context, cmd *Command) error {
	for i := range cmd.Replacements {
		nodeClaim := &v1.NodeClaim{}
		if err := q.apiReader.Get(ctx, types.NamespacedName{Name: cmd.Replacements[i].Name}, nodeClaim); err != nil {
			if errors.IsNotFound(err) && !q.cluster.NodeClaimExists(cmd.Replacements[i].Name) {
				return NewUnrecoverableError(fmt.Errorf("initialized replacement was deleted, %w", err))
			}
			return fmt.Errorf("verifying replacement initialization, %w", err)
		}
		if !nodeClaim.DeletionTimestamp.IsZero() {
			return NewUnrecoverableError(fmt.Errorf("initialized replacement is terminating"))
		}
		if !nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue() {
			cmd.Replacements[i].Initialized = false
			return fmt.Errorf("verifying replacement initialization, nodeclaim is not initialized")
		}

		var nodeName string
		for stateNode := range q.cluster.Nodes() {
			if stateNode.NodeClaim != nil && stateNode.NodeClaim.Name == nodeClaim.Name && stateNode.Node != nil {
				nodeName = stateNode.Node.Name
				break
			}
		}
		if nodeName == "" {
			return serrors.Wrap(fmt.Errorf("verifying replacement node readiness, node state is not available"), "NodeClaim", klog.KObj(nodeClaim))
		}
		node := &corev1.Node{}
		if err := q.apiReader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			return fmt.Errorf("verifying replacement node readiness, %w", err)
		}
		if !node.DeletionTimestamp.IsZero() {
			return NewUnrecoverableError(fmt.Errorf("initialized replacement node is terminating"))
		}
		if nodeutils.GetCondition(node, corev1.NodeReady).Status != corev1.ConditionTrue {
			return fmt.Errorf("verifying replacement node readiness, node is not ready")
		}
	}
	return nil
}

// isMultiNodeMultiReplacementCommand returns true only for the alpha bounded multi-node multi-replacement command
// shape: the feature gate is enabled, the command came from multi-node consolidation, and it launches exactly two
// replacements. Everything guarded by this predicate is a no-op while the feature gate is disabled.
func isMultiNodeMultiReplacementCommand(ctx context.Context, cmd *Command) bool {
	return options.FromContext(ctx).FeatureGates.MultiNodeMultiReplacement &&
		cmd.Method != nil &&
		cmd.ConsolidationType() == MultiNodeConsolidationType &&
		len(cmd.Replacements) == maxMultiNodeMultiReplacements
}

// StartCommand will do the following:
// 1. Taint candidate nodes
// 2. Spin up replacement nodes
// 3. Add Command to the queue to wait to delete the candidates.
func (q *Queue) StartCommand(ctx context.Context, cmd *Command) error {
	// First check if we can add the command.
	providerIDs := lo.Map(cmd.Candidates, func(c *Candidate, _ int) string {
		return c.ProviderID()
	})
	if q.HasAny(providerIDs...) {
		return fmt.Errorf("candidate is being disrupted")
	}

	log.FromContext(ctx).WithValues(append([]any{
		"command", cmd.String(),
	}, cmd.LogValues()...)...).Info("disrupting node(s)")

	// Cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	markedCandidates, taintedCandidates, markDisruptedErr := q.markDisrupted(ctx, cmd)
	// If we get a failure marking some nodes as disrupted, if we are launching replacements, we shouldn't continue
	// with disrupting the candidates. If it's just a delete operation, we can proceed
	if markDisruptedErr != nil && (len(cmd.Replacements) > 0 || len(markedCandidates) == 0) {
		return serrors.Wrap(fmt.Errorf("marking disrupted, %w", multierr.Combine(markDisruptedErr, q.rollbackCandidates(ctx, markedCandidates, taintedCandidates))), "command-id", cmd.ID)
	}

	// Update the command to only consider the successfully MarkDisrupted candidates
	cmd.Candidates = markedCandidates
	// A partially marked candidate is no longer part of the command, so drop the taint that was added to it.
	if partial := lo.Reject(taintedCandidates, func(candidate *Candidate, _ int) bool {
		return lo.Contains(markedCandidates, candidate)
	}); len(partial) > 0 {
		if err := q.rollbackCandidates(ctx, nil, partial); err != nil {
			log.FromContext(ctx).Error(err, "failed rolling back partially marked candidates")
		}
	}
	cmd.taintedCandidates = lo.Filter(taintedCandidates, func(candidate *Candidate, _ int) bool {
		return lo.Contains(markedCandidates, candidate)
	})

	if err := q.createReplacementNodeClaims(ctx, cmd); err != nil {
		// If we failed to launch the replacement, don't disrupt.  If this is some permanent failure,
		// we don't want to disrupt workloads with no way to provision new nodes for them.
		cleanupErr := multierr.Combine(q.rollbackCandidates(ctx, cmd.Candidates, cmd.taintedCandidates), q.cleanupUnusedReplacements(ctx, cmd))
		return serrors.Wrap(fmt.Errorf("launching replacement nodeclaim, %w", multierr.Combine(err, cleanupErr)), "command-id", cmd.ID)
	}
	// IMPORTANT
	// We must MarkForDeletion AFTER we launch the replacements and not before
	// The reason for this is to avoid producing double-launches
	// If we MarkForDeletion before we create replacements, it's possible for the provisioner
	// to recognize that it needs to launch capacity for terminating pods, causing us to launch
	// capacity for these pods twice instead of just once
	q.cluster.MarkForDeletion(lo.Map(cmd.Candidates, func(c *Candidate, _ int) string { return c.ProviderID() })...)

	// Nominate each node for scheduling and emit pod nomination events
	// We emit all nominations before we exit the disruption loop as
	// we want to ensure that nodes that are nominated are respected in the subsequent
	// disruption reconciliation. This is essential in correctly modeling multiple
	// disruption commands in parallel.
	// This will only nominate nodes for 2 * batchingWindow. Once the candidates are
	// tainted with the Karpenter taint, the provisioning controller will continue
	// to do scheduling simulations and nominate the pods on the candidate nodes until
	// the node is cleaned up.
	cmd.Results.Record(log.IntoContext(ctx, operatorlogging.NopLogger), q.recorder, q.cluster)

	q.Lock()
	for _, c := range cmd.Candidates {
		q.ProviderIDToCommand[c.ProviderID()] = cmd
	}
	// IMPORTANT
	// We are adding the first nodeclaim in the list of candidates into the reconciliation queue
	// This invariant SHOULD NOT be relied on anywhere else besides within this file.
	q.source <- event.TypedGenericEvent[*v1.NodeClaim]{Object: cmd.Candidates[0].NodeClaim}
	q.Unlock()

	// An action is only performed and pods/nodes are only disrupted after a successful add to the queue
	nodePools := lo.Uniq(lo.Map(cmd.Candidates, func(c *Candidate, _ int) string {
		return c.NodePool.Name
	}))
	for _, nodePool := range nodePools {
		NodepoolDecisionsPerformed.Inc(map[string]string{
			metrics.NodePoolLabel:  nodePool,
			decisionLabel:          string(cmd.Decision()),
			metrics.ReasonLabel:    strings.ToLower(string(cmd.Reason())),
			ConsolidationTypeLabel: cmd.ConsolidationType(),
		})
	}
	DecisionsPerformedTotal.Inc(map[string]string{
		decisionLabel:          string(cmd.Decision()),
		metrics.ReasonLabel:    strings.ToLower(string(cmd.Reason())),
		ConsolidationTypeLabel: cmd.ConsolidationType(),
	})
	return nil
}

// HasAny checks to see if the candidate is part of an currently executing command.
func (q *Queue) HasAny(ids ...string) bool {
	q.RLock()
	defer q.RUnlock()

	// If the mapping has at least one of the candidates' providerIDs, return true.
	for _, id := range ids {
		if _, exists := q.ProviderIDToCommand[id]; exists {
			return true
		}
	}
	return false
}

// For TESTING ONLY
// This function is not thread safe as it returns pointers to commands.
// If you edit these commands returned, you can create race conditions.
func (q *Queue) GetCommands() []*Command {
	q.RLock()
	defer q.RUnlock()

	return lo.UniqValues(q.ProviderIDToCommand)
}

// CompleteCommand fully clears the queue of all references of a hash/command
func (q *Queue) CompleteCommand(cmd *Command) {
	if !cmd.Succeeded {
		q.cluster.UnmarkForDeletion(lo.Map(cmd.Candidates, func(c *Candidate, _ int) string { return c.ProviderID() })...)
	}
	// Remove all candidates linked to the command
	q.Lock()
	defer q.Unlock()
	for _, c := range cmd.Candidates {
		delete(q.ProviderIDToCommand, c.ProviderID())
	}
}

func (q *Queue) IsEmpty() bool {
	q.RLock()
	defer q.RUnlock()
	return len(q.ProviderIDToCommand) == 0
}
