/*
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

package orchestration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/avast/retry-go"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

// waitRetryOptions are the retry options used when trying make a successful delete call on a nodeClaim after its
// replacements have been spun up.
func waitRetryOptions(ctx context.Context) []retry.Option {
	return []retry.Option{
		retry.Context(ctx),
		retry.Delay(2 * time.Second),
		retry.LastErrorOnly(true),
		retry.Attempts(10),
		retry.MaxDelay(10 * time.Second), // 22 + (10-5)*10 =~ 1 minute in total
	}
}

const (
	queueBaseDelay   = 1 * time.Second
	queueMaxDelay    = 10 * time.Second
	maxRetryDuration = 10 * time.Minute
)

type Command struct {
	ReplacementKeys []*NodeClaimKey
	Candidates      []*state.StateNode
	Reason          string    // reason is used for metrics
	TimeAdded       time.Time // timeAdded is used to track timeouts
	LastError       error
}

// NodeClaimKey wraps a nodeclaim.Key with an initialized field to save on readiness checks and identify
// when a nodeclaim is first initialized for metrics and events.
type NodeClaimKey struct {
	nodeclaim.Key
	// Use a bool track if a node has already been initialized. This intentionally does not capture nodes that go
	// initialized then go NotReady after as other pods can schedule to this node as well.
	Initialized bool
}

type Queue struct {
	workqueue.RateLimitingInterface
	// providerID -> command, maps a candidate to its command. Each command has a list of candidates that can be used
	// to map to itself.
	candidateProviderIDToCommand map[string]*Command

	kubeClient client.Client
	recorder   events.Recorder
	cluster    *state.Cluster
	clock      clock.Clock
}

// NewQueue creates a queue that will asynchronously orchestrate deprovisioning commands
func NewQueue(ctx context.Context, kubeClient client.Client, recorder events.Recorder, cluster *state.Cluster, clock clock.Clock, testingMode bool) *Queue {
	queue := &Queue{
		RateLimitingInterface:        workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(queueBaseDelay, queueMaxDelay)),
		candidateProviderIDToCommand: map[string]*Command{},
		kubeClient:                   kubeClient,
		recorder:                     recorder,
		cluster:                      cluster,
		clock:                        clock,
	}
	// If testing, we don't actually need to start the queue. We can call each function individually instead of relying
	// on the rate limiting interface itself.
	if !testingMode {
		go queue.Start(logging.WithLogger(ctx, logging.FromContext(ctx).Named("deprovisioning")))
	}
	return queue
}

// NewCommand creates a command key and adds in initial data for the orchestration queue.
func NewCommand(replacements []nodeclaim.Key, candidates []*state.StateNode, reason string, timeAdded time.Time) *Command {
	return &Command{
		ReplacementKeys: lo.Map(replacements, func(key nodeclaim.Key, _ int) *NodeClaimKey {
			return &NodeClaimKey{Key: key}
		}),
		Candidates: candidates,
		Reason:     reason,
		TimeAdded:  timeAdded,
	}
}

// Start will kick off the queue. This runs asynchronously until Karpenter shuts down.
func (q *Queue) Start(ctx context.Context) {
Loop:
	for {
		select {
		case <-ctx.Done():
			break Loop
		default:
			cmd, shutdown := q.Pop()
			if shutdown {
				break Loop
			}
			q.ProcessItem(ctx, cmd)
		}
	}
	logging.FromContext(ctx).Errorf("deprovisioning queue is broken and has shutdown")
}

// Pop returns a command and its hash and if the queue has shutdown.
func (q *Queue) Pop() (*Command, bool) {
	// Get command from queue. This waits until queue is non-empty.
	item, shutdown := q.RateLimitingInterface.Get()
	if shutdown {
		return &Command{}, true
	}
	cmd := item.(*Command)
	return cmd, false
}

// ProcessItem will process a command. It will either:
// 1. Remove all references of the command if the command completes due to timeout or success.
// 2. Check the command and requeue if it needs to be checked again later.
func (q *Queue) ProcessItem(ctx context.Context, cmd *Command) {
	requeue, err := q.Handle(ctx, cmd)
	if !requeue {
		// If the command timed out, log the last error.
		if err != nil {
			// If the command failed, bail on the action.
			// 1. Emit metrics for launch failures
			// 2. Ensure cluster state no longer thinks these nodes are deleting
			// 3. Remove it from the Queue's internal data structure
			deprovisioningReplacementNodeLaunchFailedCounter.WithLabelValues(cmd.Reason).Inc()
			q.cluster.UnmarkForDeletion(lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string { return s.ProviderID() })...)
			err = multierr.Append(err, q.setNodesUnschedulable(ctx, false, lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string { return s.Node.Name })...))
			logging.FromContext(ctx).Debugf("handling deprovisioning command, %w, for nodes %s", err,
				strings.Join(lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string {
					return s.Name()
				}), ","))
		}
		q.Remove(cmd)
		return
	}
	q.RateLimitingInterface.Done(cmd)
	// Requeue command if not complete
	q.RateLimitingInterface.AddRateLimited(cmd)
}

// Add adds commands to the Queue
// Each command added to the queue should already be validated and ready for execution.
func (q *Queue) Add(cmd *Command) error {
	providerIDs := lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string {
		return s.ProviderID()
	})
	// First check if we can add the command.
	if err := q.CanAdd(providerIDs...); err != nil {
		return fmt.Errorf("adding command, %w", err)
	}
	for _, candidate := range cmd.Candidates {
		q.candidateProviderIDToCommand[candidate.ProviderID()] = cmd
	}
	// Idempotently mark for deletion
	q.cluster.MarkForDeletion(providerIDs...)
	q.RateLimitingInterface.Add(cmd)
	return nil
}

// Remove fully clears the queue of all references of a hash/command
func (q *Queue) Remove(cmd *Command) {
	// Remove all candidates linked to the command
	for _, candidate := range cmd.Candidates {
		delete(q.candidateProviderIDToCommand, candidate.ProviderID())
	}
	q.RateLimitingInterface.Forget(cmd)
	q.RateLimitingInterface.Done(cmd)
}

// CanAdd is a quick check to see if the candidate is already part of a deprovisioning action
func (q *Queue) CanAdd(ids ...string) error {
	var err error
	for _, id := range ids {
		if _, ok := q.candidateProviderIDToCommand[id]; ok {
			err = multierr.Append(err, fmt.Errorf("candidate is part of active deprovisioning decision"))
		}
	}
	return err
}

// Handle will check for timeouts, then execute a wait or termination.
func (q *Queue) Handle(ctx context.Context, cmd *Command) (bool, error) {
	if q.clock.Since(cmd.TimeAdded) > maxRetryDuration {
		return false, fmt.Errorf("abandoning command, reached timeout, %w", cmd.LastError)
	}
	// If the time hasn't expired, either wait, or terminate.
	requeue, err := q.WaitOrTerminate(ctx, cmd)
	if err != nil {
		// If there was an error, set this as the command's last error so that we can propagate it.
		cmd.LastError = err
		if !retry.IsRecoverable(err) {
			return false, nil
		}
		return requeue, nil
	}
	return requeue, nil
}

// WaitOrTerminate will wait until launched machines are ready.
// Once the replacements are ready, it will terminate the candidates.
// Will return true if the item in the queue should be re-queued. If a command has
// timed out, this will return false.
// nolint:gocyclo
func (q *Queue) WaitOrTerminate(ctx context.Context, cmd *Command) (bool, error) {
	waitErrs := make([]error, len(cmd.ReplacementKeys))
	for i := range cmd.ReplacementKeys {
		key := cmd.ReplacementKeys[i]
		// If we know the node claim is initialized, no need to check again.
		if key.Initialized {
			continue
		}
		// Get the nodeclaim
		nodeClaim, err := nodeclaim.Get(ctx, q.kubeClient, key.Key)
		if err != nil {
			// The NodeClaim got deleted after an initial eventual consistency delay
			// This means that there was an ICE error or the Node initializationTTL expired
			// In this case, the error is unrecoverable, so don't requeue.
			if errors.IsNotFound(err) && q.clock.Since(cmd.TimeAdded) > time.Second*5 {
				return false, retry.Unrecoverable(fmt.Errorf("getting machine, %w", err))
			}
			waitErrs[i] = fmt.Errorf("getting node claim, %w", err)
			continue
		}
		// We emitted this event when Deprovisioning was blocked on launching/termination.
		// This does not block other forms of deprovisioning, but we should still emit this.
		q.recorder.Publish(deprovisioningevents.Launching(nodeClaim, cmd.Reason))
		initializedStatus := nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized)
		if !initializedStatus.IsTrue() {
			q.recorder.Publish(deprovisioningevents.WaitingOnReadiness(nodeClaim))
			waitErrs[i] = fmt.Errorf("getting node claim, %w", err)
			continue
		}
		key.Initialized = true
		// Subtract the last initialization time from the time the command was added to get initialization duration.
		initLength := initializedStatus.LastTransitionTime.Inner.Time.Sub(cmd.TimeAdded).Seconds()
		deprovisioningReplacementNodeInitializedHistogram.Observe(initLength)
	}
	// If we have any errors, don't continue
	if err := multierr.Combine(waitErrs...); err != nil {
		return true, err
	}

	// All replacements have been provisioned.
	// All we need to do now is get a successful delete call for each node claim,
	// then the termination controller will handle the eventual deletion of the nodes.
	var multiErr error
	for i := range cmd.Candidates {
		candidate := cmd.Candidates[i]
		q.recorder.Publish(deprovisioningevents.Terminating(candidate.Node, candidate.NodeClaim, cmd.Reason)...)
		if err := nodeclaim.Delete(ctx, q.kubeClient, candidate.NodeClaim); err != nil {
			if !errors.IsNotFound(err) {
				multiErr = multierr.Append(multiErr, err)
			}
		} else {
			nodeclaim.TerminatedCounter(cmd.Candidates[i].NodeClaim, cmd.Reason).Inc()
		}
	}
	// If there were any deletion failures, we should requeue.
	// In the case where we requeue, but the timeout for the command is reached, we'll mark this as a failure.
	if multiErr != nil {
		return false, fmt.Errorf("terminating nodeclaims, %w", multiErr)
	}
	return false, nil
}

func (q *Queue) setNodesUnschedulable(ctx context.Context, isUnschedulable bool, candidates ...string) error {
	var multiErr error
	for _, cn := range candidates {
		node := &v1.Node{}
		if err := q.kubeClient.Get(ctx, client.ObjectKey{Name: cn}, node); err != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("getting node, %w", err))
		}

		// node is being deleted already, so no need to un-cordon
		if !isUnschedulable && !node.DeletionTimestamp.IsZero() {
			continue
		}

		// already matches the state we want to be in
		if node.Spec.Unschedulable == isUnschedulable {
			continue
		}

		stored := node.DeepCopy()
		node.Spec.Unschedulable = isUnschedulable
		if err := q.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("patching node %s, %w", node.Name, err))
		}
	}
	return multiErr
}

// Reset is used for testing and clears all internal data structures
func (q *Queue) Reset() {
	q.RateLimitingInterface = workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(queueBaseDelay, queueMaxDelay))
	q.candidateProviderIDToCommand = map[string]*Command{}
}
