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
	"github.com/mitchellh/hashstructure/v2"
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
	queueBaseDelay   = 100 * time.Millisecond
	queueMaxDelay    = 10 * time.Second
	maxRetryDuration = 10 * time.Minute
)

type Command struct {
	ReplacementKeys []*nodeClaimKey
	Candidates      []*state.StateNode
	Reason          string    `hash:"ignore"` // reason is used for metrics
	timeAdded       time.Time `hash:"ignore"`
	lastError       error
}

type nodeClaimKey struct {
	nodeclaim.Key
	initialized bool // wrap a bool so we can save on recurring calls to check for initialization
}

type Queue struct {
	workqueue.RateLimitingInterface
	candidateProviderIDToCommandID map[string]uint64  // providerID -> commandID, used for quick checks to see if an individual candidate is in queue
	idToCommand                    map[uint64]*Command // commandID -> Command, used to associate a unique identifier to the Command details.

	kubeClient client.Client
	recorder   events.Recorder
	cluster    *state.Cluster
	clock      clock.Clock
}

func NewQueue(ctx context.Context, kubeClient client.Client, recorder events.Recorder, cluster *state.Cluster) *Queue {
	queue := &Queue{
		RateLimitingInterface:          workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(queueBaseDelay, queueMaxDelay)),
		candidateProviderIDToCommandID: map[string]uint64{},
		idToCommand:                    map[uint64]*Command{},
		kubeClient:                     kubeClient,
		recorder:                       recorder,
		cluster:                        cluster,
	}
	go queue.Start(logging.WithLogger(ctx, logging.FromContext(ctx).Named("disruption")))
	return queue
}

// NewCommand creates a command key and adds in initial data for the orchestration queue.
func NewCommand(replacements []nodeclaim.Key, candidates []*state.StateNode, reason string, timeAdded time.Time) *Command {
	return &Command {
		ReplacementKeys: lo.Map(replacements, func(key nodeclaim.Key, _ int) *nodeClaimKey {
			return &nodeClaimKey{Key: key}
		}),
		Candidates: candidates,
		Reason: reason,
		timeAdded: timeAdded,
	}
}

// Start will kick off the queue. This runs asynchronously unil Karpenter shuts down.
func (q *Queue) Start(ctx context.Context) {
	for {
		// Get command from queue. This waits until queue is non-empty.
		item, shutdown := q.RateLimitingInterface.Get()
		if shutdown {
			break
		}
		hash := item.(uint64)
		cmd := q.idToCommand[hash]
		requeue, err := q.Handle(ctx, hash, cmd)
		if !requeue {
			// If we're not requeuing and the most recent function call had an error, return the last error.
			if err != nil {
				if err := q.setNodesUnschedulable(ctx, false, lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string { return s.Node.Name })...); err != nil {
					logging.FromContext(ctx).Debugf("un-cordoning nodes, %w", err) // requeue so we can try to un-cordon again
				}
				logging.FromContext(ctx).Debugf("handling deprovisioning command, %w, for nodes %s", err,
				strings.Join(lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string {
					return s.Name()
				}), ","))
			}
			q.Remove(hash, cmd)
			continue
		}
		q.RateLimitingInterface.Done(hash)
		// Requeue command if not complete
		q.RateLimitingInterface.AddRateLimited(hash)
	}
	logging.FromContext(ctx).Errorf("deprovisioning queue is broken and has shutdown")
}

// Add adds commands to the Queue
// Each command added to the queue should already be validated and ready for execution.
func (q *Queue) Add(cmd *Command) error {
	// First check if we can add the command.
	if err := q.CanAdd(lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string {
		return s.ProviderID()
	})...); err != nil {
		return fmt.Errorf("adding command, %w", err)
	}
	// Hash the command so that we can connect the two underlying data structures
	// This only needs to be done once so we can make a logical connection.
	hash, err := hashstructure.Hash(cmd, hashstructure.FormatV2, &hashstructure.HashOptions{
		SlicesAsSets:    true,
		IgnoreZeroValue: true,
		ZeroNil:         true,
	})
	if err != nil {
		return fmt.Errorf("hashing command, %w", err)
	}
	for _, candidate := range cmd.Candidates {
		q.candidateProviderIDToCommandID[candidate.ProviderID()] = hash
		q.idToCommand[hash] = cmd
	}
	q.RateLimitingInterface.Add(hash)
	return nil
}

// CanAdd is a quick check to see if the candidate is already part of a deprovisioning action
func (q *Queue) CanAdd(ids ...string) error {
	var err error
	for _, id := range ids {
		if _, ok := q.candidateProviderIDToCommandID[id]; ok {
			err = multierr.Append(err, fmt.Errorf("candidate is part of active deprovisioning decision"))
		}
	}
	return err
}

// Handle will check for timeouts, then execute a wait or termination.
// If there
func (q *Queue) Handle(ctx context.Context, hash uint64, cmd *Command) (bool, error) {
	// If the item has surpassed the max duration, bail on the action.
	// 1. Emit metrics for launch failures
	// 2. Ensure cluster state no longer thinks these nodes are deleting
	// 3. Un-cordon the nodes we previously cordoned
	// 4. Remove it from the Queue's internal data structure
	if time.Since(cmd.timeAdded) > maxRetryDuration {
		deprovisioningReplacementNodeLaunchFailedCounter.WithLabelValues(cmd.Reason).Inc()
		q.cluster.UnmarkForDeletion(lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string { return s.ProviderID() })...)
		return false, fmt.Errorf("reaching timeout, %w", cmd.lastError)
	}
	// If the time hasn't expired, either wait, or terminate.
	requeue, err := q.WaitOrTerminate(ctx, cmd)
	if err != nil {
		// If there was an error, set this as the command's last error so that we can propagate it.
		cmd.lastError = err
		if !retry.IsRecoverable(err) {
			return false, nil
		}
		return requeue, nil
	}
	return requeue, nil
}

// Remove fully clears the queue of all references of a hash/command
func (q *Queue) Remove(hash uint64, cmd *Command) {
	q.RateLimitingInterface.Forget(hash)
	for _, candidate := range cmd.Candidates {
		delete(q.candidateProviderIDToCommandID, candidate.ProviderID())
	}
	delete(q.idToCommand, hash)
	q.RateLimitingInterface.Done(hash)
}

// WaitOrTerminate will wait until launched machines are ready.
// Once the replacements are ready, it will terminate the candidates.
// Will return true if the item in the queue should be re-queued. If a command has
// timed out, this will return false.
// nolint:gocyclo
func (q *Queue) WaitOrTerminate(ctx context.Context, cmd *Command) (bool, error) {
	var waitErrs []error
	for i := range cmd.ReplacementKeys {
		key := cmd.ReplacementKeys[i]
		// If we know the node claim is initialized, no need to check again.
		if key.initialized {
			continue
		}
		// Get the nodeclaim
		nodeClaim, err := nodeclaim.Get(ctx, q.kubeClient, key.Key)
		if err != nil {
			// The NodeClaim got deleted after an initial eventual consistency delay
			// This means that there was an ICE error or the Node initializationTTL expired
			// In this case, the error is unrecoverable, so don't requeue.
			if errors.IsNotFound(err) && q.clock.Since(cmd.timeAdded) > time.Second*5 {
				return false, retry.Unrecoverable(fmt.Errorf("getting machine, %w", err))
			}
			waitErrs[i] = fmt.Errorf("getting node claim, %w", err)
			continue
		}
		// We emitted this event when Deprovisioning was blocked on launching/termination.
		// This does not block other forms of deprovisioning, but we should still emit this.
		q.recorder.Publish(deprovisioningevents.Launching(nodeClaim, cmd.Reason))
		if !nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized).IsTrue() {
			q.recorder.Publish(deprovisioningevents.WaitingOnReadiness(nodeClaim))
			waitErrs[i] = fmt.Errorf("getting node claim, %w", err)
			continue
		}
		key.initialized = true
		// This should only be reached once since initialization is checked at the beginning.
		deprovisioningReplacementNodeInitializedHistogram.Observe(time.Since(cmd.timeAdded).Seconds())
	}
	// If we have any errors, don't continue
	if err := multierr.Combine(waitErrs...); err != nil {
		return true, err
	}

	// Reaching here means we know that all replacements have been provisioned.
	// All we need to do now is get a successful delete call for each node claim,
	// then the termination controller will handle the eventual deletion of the nodes.
	var errs []error
	workqueue.ParallelizeUntil(ctx, len(cmd.Candidates), len(cmd.Candidates), func(i int) {
		q.recorder.Publish(deprovisioningevents.Terminating(cmd.Candidates[i].Node, cmd.Candidates[i].NodeClaim, cmd.Reason)...)
		errs[i] = retry.Do(func() error {
			if err := nodeclaim.Delete(ctx, q.kubeClient, cmd.Candidates[i].NodeClaim); err != nil {
				if !errors.IsNotFound(err) {
					logging.FromContext(ctx).Errorf("terminating machine, %s", err)
					return err
				}
			}
			return nil
		}, waitRetryOptions(ctx)...)
		nodeclaim.TerminatedCounter(cmd.Candidates[i].NodeClaim, cmd.Reason).Inc()
	})
	// If there were any deletion failures, we should requeue.
	// In the case where we requeue, but the timeout for the command is reached, we'll
	return true, multierr.Combine(errs...)
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

// Reset is used for testing and clears all clears all internal data structures
func (q *Queue) Reset() {
	for _, hash := range q.idToCommand {
		q.Forget(hash)
		q.Done(hash)
	}
	q.candidateProviderIDToCommandID = map[string]uint64{}
	q.idToCommand = map[uint64]*Command{}
	q.cluster.Reset()
}
