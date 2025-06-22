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
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	operatorlogging "sigs.k8s.io/karpenter/pkg/operator/logging"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

const (
	queueBaseDelay   = 1 * time.Second
	queueMaxDelay    = 10 * time.Second
	maxRetryDuration = 10 * time.Minute
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

type Queue struct {
	workqueue.TypedRateLimitingInterface[*Command]

	mu                  sync.RWMutex
	providerIDToCommand map[string]*Command // providerID -> command, maps a candidate to its command

	kubeClient  client.Client
	recorder    events.Recorder
	cluster     *state.Cluster
	clock       clock.Clock
	provisioner *provisioning.Provisioner
}

// NewQueue creates a queue that will asynchronously orchestrate disruption commands
func NewQueue(kubeClient client.Client, recorder events.Recorder, cluster *state.Cluster, clock clock.Clock,
	provisioner *provisioning.Provisioner,
) *Queue {
	queue := &Queue{
		// nolint:staticcheck
		// We need to implement a deprecated interface since Command currently doesn't implement "comparable"
		TypedRateLimitingInterface: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[*Command](queueBaseDelay, queueMaxDelay),
			workqueue.TypedRateLimitingQueueConfig[*Command]{
				Name: "disruption.workqueue",
			}),
		providerIDToCommand: map[string]*Command{},
		kubeClient:          kubeClient,
		recorder:            recorder,
		cluster:             cluster,
		clock:               clock,
		provisioner:         provisioner,
	}
	return queue
}

func (q *Queue) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("disruption.queue").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(q))
}

func (q *Queue) Reconcile(ctx context.Context) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "disruption.queue")

	// Check if the queue is empty. client-go recommends not using this function to gate the subsequent
	// get call, but since we're popping items off the queue synchronously retrying, there should be
	// no synchonization issues.
	if q.Len() == 0 {
		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Get command from queue. This waits until queue is non-empty.
	cmd, shutdown := q.TypedRateLimitingInterface.Get()
	if shutdown {
		panic("unexpected failure, disruption queue has shut down")
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues(cmd.LogValues()...))

	if err := q.waitOrTerminate(ctx, cmd); err != nil {
		// If recoverable, re-queue and try again.
		if !IsUnrecoverableError(err) {
			// mark this item as done processing. This is necessary so that the RLI is able to add the item back in.
			q.TypedRateLimitingInterface.Done(cmd)
			q.TypedRateLimitingInterface.AddRateLimited(cmd)
			return reconcile.Result{RequeueAfter: singleton.RequeueImmediately}, nil
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
			ConsolidationTypeLabel: cmd.ConsolidationType(),
		})
		stateNodes := lo.Map(cmd.Candidates, func(c *Candidate, _ int) *state.StateNode { return c.StateNode })
		multiErr := multierr.Combine(err, state.RequireNoScheduleTaint(ctx, q.kubeClient, false, stateNodes...))
		multiErr = multierr.Combine(multiErr, state.ClearNodeClaimsCondition(ctx, q.kubeClient, v1.ConditionTypeDisruptionReason, stateNodes...))
		// Log the error
		log.FromContext(ctx).Error(multiErr, "failed terminating nodes while executing a disruption command")
	} else {
		log.FromContext(ctx).V(1).Info("command succeeded")
		cmd.Succeeded = true
	}
	q.CompleteCommand(cmd)
	return reconcile.Result{RequeueAfter: singleton.RequeueImmediately}, nil
}

// waitOrTerminate will wait until launched nodeclaims are ready.
// Once the replacements are ready, it will terminate the candidates.
// nolint:gocyclo
func (q *Queue) waitOrTerminate(ctx context.Context, cmd *Command) (err error) {
	// Wrap an error in an unrecoverable error if it timed out
	defer func() {
		if q.clock.Since(cmd.CreationTimestamp) > maxRetryDuration {
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
		metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
			metrics.ReasonLabel:       pretty.ToSnakeCase(string(cmd.Reason())),
			metrics.NodePoolLabel:     cmd.Candidates[i].NodeClaim.Labels[v1.NodePoolLabelKey],
			metrics.CapacityTypeLabel: cmd.Candidates[i].NodeClaim.Labels[v1.CapacityTypeLabelKey],
		})
	})
	// If there were any deletion failures, we should requeue.
	// In the case where we requeue, but the timeout for the command is reached, we'll mark this as a failure.
	return multierr.Combine(errs...)
}

// markDisrupted taints the node and adds the Disrupted condition to the NodeClaim for a candidate that is about to be disrupted
func (q *Queue) markDisrupted(ctx context.Context, cmd *Command) ([]*Candidate, error) {
	errs := make([]error, len(cmd.Candidates))
	workqueue.ParallelizeUntil(ctx, len(cmd.Candidates), len(cmd.Candidates), func(i int) {
		if err := state.RequireNoScheduleTaint(ctx, q.kubeClient, true, cmd.Candidates[i].StateNode); err != nil {
			errs[i] = serrors.Wrap(fmt.Errorf("tainting nodes, %w", err), "taint", pretty.Taint(v1.DisruptedNoScheduleTaint))
			return
		}
		// refresh nodeclaim before updating status
		nodeClaim := &v1.NodeClaim{}
		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return client.IgnoreNotFound(err) != nil }, func() error {
			return q.kubeClient.Get(ctx, client.ObjectKeyFromObject(cmd.Candidates[i].NodeClaim), nodeClaim)
		}); err != nil {
			errs[i] = client.IgnoreNotFound(err)
			return
		}
		stored := nodeClaim.DeepCopy()
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeDisruptionReason, string(cmd.Reason()), string(cmd.Reason()))
		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return client.IgnoreNotFound(err) != nil }, func() error { return q.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored)) }); err != nil {
			errs[i] = client.IgnoreNotFound(err)
			return
		}
	})
	var markedCandidates []*Candidate
	for i := range errs {
		if errs[i] != nil {
			continue
		}
		markedCandidates = append(markedCandidates, cmd.Candidates[i])
	}
	return markedCandidates, multierr.Combine(errs...)
}

// createReplacementNodeClaims creates replacement NodeClaims
func (q *Queue) createReplacementNodeClaims(ctx context.Context, cmd *Command) error {
	nodeClaimNames, err := q.provisioner.CreateNodeClaims(ctx, lo.Map(cmd.Replacements, func(r *Replacement, _ int) *pscheduling.NodeClaim { return r.NodeClaim }), provisioning.WithReason(strings.ToLower(string(cmd.Reason()))))
	if err != nil {
		return err
	}
	if len(nodeClaimNames) != len(cmd.Replacements) {
		// shouldn't ever occur since a partially failed CreateNodeClaims should return an error
		return serrors.Wrap(fmt.Errorf("expected replacement count did not equal actual replacement count"), "expected-count", len(cmd.Replacements), "actual-count", len(nodeClaimNames))
	}
	for i, name := range nodeClaimNames {
		cmd.Replacements[i].Name = name
	}
	return nil
}

// StartCommand will do the following:
// 1. Taint candidate nodes
// 2. Spin up replacement nodes
// 3. Add Command to the queue to wait to delete the candidates.
func (q *Queue) StartCommand(ctx context.Context, cmd *Command) error {
	// First check if we can add the command.
	if q.HasAny(lo.Map(cmd.Candidates, func(c *Candidate, _ int) string { return c.ProviderID() })...) {
		return fmt.Errorf("candidate is being disrupted")
	}

	log.FromContext(ctx).WithValues(append([]any{"command-id", cmd.ID, "reason", strings.ToLower(string(cmd.Reason()))}, cmd.LogValues()...)...).Info("disrupting node(s)")

	// Cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	markedCandidates, markDisruptedErr := q.markDisrupted(ctx, cmd)
	// If we get a failure marking some nodes as disrupted, if we are launching replacements, we shouldn't continue
	// with disrupting the candidates. If it's just a delete operation, we can proceed
	if markDisruptedErr != nil && len(cmd.Replacements) > 0 {
		return serrors.Wrap(fmt.Errorf("marking disrupted, %w", markDisruptedErr), "command-id", cmd.ID)
	}

	// Update the command to only consider the successfully MarkDisrupted candidates
	cmd.Candidates = markedCandidates

	if err := q.createReplacementNodeClaims(ctx, cmd); err != nil {
		// If we failed to launch the replacement, don't disrupt.  If this is some permanent failure,
		// we don't want to disrupt workloads with no way to provision new nodes for them.
		return serrors.Wrap(fmt.Errorf("launching replacement nodeclaim, %w", err), "command-id", cmd.ID)
	}

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

	q.mu.Lock()
	for _, candidate := range cmd.Candidates {
		q.providerIDToCommand[candidate.ProviderID()] = cmd
	}
	q.mu.Unlock()

	// IMPORTANT
	// We must MarkForDeletion AFTER we launch the replacements and not before
	// The reason for this is to avoid producing double-launches
	// If we MarkForDeletion before we create replacements, it's possible for the provisioner
	// to recognize that it needs to launch capacity for terminating pods, causing us to launch
	// capacity for these pods twice instead of just once
	q.cluster.MarkForDeletion(lo.Map(cmd.Candidates, func(c *Candidate, _ int) string { return c.ProviderID() })...)
	q.TypedRateLimitingInterface.Add(cmd)
	// An action is only performed and pods/nodes are only disrupted after a successful add to the queue
	DecisionsPerformedTotal.Inc(map[string]string{
		decisionLabel:          string(cmd.Decision()),
		metrics.ReasonLabel:    strings.ToLower(string(cmd.Reason())),
		ConsolidationTypeLabel: cmd.ConsolidationType(),
	})
	return nil
}

// HasAny checks to see if the candidate is part of an currently executing command.
func (q *Queue) HasAny(ids ...string) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()

	// If the mapping has at least one of the candidates' providerIDs, return true.
	_, ok := lo.Find(ids, func(id string) bool {
		_, ok := q.providerIDToCommand[id]
		return ok
	})
	return ok
}

// CompleteCommand fully clears the queue of all references of a hash/command
func (q *Queue) CompleteCommand(cmd *Command) {
	// mark this item as done processing. This is necessary so that the RLI is able to add the item back in.
	q.TypedRateLimitingInterface.Done(cmd)
	q.TypedRateLimitingInterface.Forget(cmd)
	if !cmd.Succeeded {
		q.cluster.UnmarkForDeletion(lo.Map(cmd.Candidates, func(c *Candidate, _ int) string { return c.ProviderID() })...)
	}
	// Remove all candidates linked to the command
	q.mu.Lock()
	for _, candidate := range cmd.Candidates {
		delete(q.providerIDToCommand, candidate.ProviderID())
	}
	q.mu.Unlock()
}

func (q *Queue) IsEmpty() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.providerIDToCommand) == 0
}
