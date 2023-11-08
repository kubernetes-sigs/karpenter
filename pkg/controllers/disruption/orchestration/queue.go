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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/operator/controller"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	disruptionevents "github.com/aws/karpenter-core/pkg/controllers/disruption/events"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

const (
	queueBaseDelay   = 1 * time.Second
	queueMaxDelay    = 10 * time.Second
	maxRetryDuration = 10 * time.Minute
)

type Command struct {
	ReplacementKeys   []*NodeClaimKey
	Candidates        []*state.StateNode
	ConsolidationType string    // used for metrics
	Method            string    // used for metrics
	Reason            string    // used for metrics
	TimeAdded         time.Time // timeAdded is used to track timeouts
	LastError         error
}

// NodeClaimKey wraps a nodeclaim.Key with an initialized field to save on readiness checks and identify
// when a nodeclaim is first initialized for metrics and events.
type NodeClaimKey struct {
	nodeclaim.Key
	// Use a bool track if a node has already been initialized so we can fire metrics for intialization once.
	// This intentionally does not capture nodes that go initialized then go NotReady after as other pods can
	// schedule to this node as well.
	Initialized bool
}

type CommandExecutionError struct {
	error
}

func NewCommandExecutionError(err error) *CommandExecutionError {
	return &CommandExecutionError{error: err}
}

func IsCommandExecutionError(err error) bool {
	if err == nil {
		return false
	}
	var commandExecutionError *CommandExecutionError
	return errors.As(err, &commandExecutionError)
}

type Queue struct {
	workqueue.RateLimitingInterface
	// providerID -> command, maps a candidate to its command. Each command has a list of candidates that can be used
	// to map to itself.
	ProviderIDToCommand map[string]*Command

	kubeClient  client.Client
	recorder    events.Recorder
	cluster     *state.Cluster
	clock       clock.Clock
	provisioner *provisioning.Provisioner
}

// NewQueue creates a queue that will asynchronously orchestrate deprovisioning commands
func NewQueue(kubeClient client.Client, recorder events.Recorder, cluster *state.Cluster, clock clock.Clock,
	provisioner *provisioning.Provisioner) *Queue {
	queue := &Queue{
		RateLimitingInterface: workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(queueBaseDelay, queueMaxDelay)),
		ProviderIDToCommand:   map[string]*Command{},
		kubeClient:            kubeClient,
		recorder:              recorder,
		cluster:               cluster,
		clock:                 clock,
		provisioner:           provisioner,
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

func (q *Queue) Name() string {
	return "disruption-queue"
}

func (q *Queue) Builder(_ context.Context, m manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(m)
}

func (q *Queue) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// Check if the queue is empty. client-go recommends not using this function to gate the subsequent
	// get call, but since we're popping items off the queue synchronously, there should be no synchonization
	// issues.
	if q.Len() == 0 {
		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}
	// Get command from queue. This waits until queue is non-empty.
	item, shutdown := q.RateLimitingInterface.Get()
	if shutdown {
		return reconcile.Result{}, fmt.Errorf("disruption queue has shut down")
	}
	cmd := item.(*Command)
	defer q.RateLimitingInterface.Done(cmd)

	if err := q.Process(ctx, cmd); err != nil {
		if !IsCommandExecutionError(err) {
			cmd.LastError = err
			q.RateLimitingInterface.AddRateLimited(cmd)
			return reconcile.Result{RequeueAfter: controller.Immediately}, nil
		}
		// If the command failed, bail on the action.
		// 1. Emit metrics for launch failures
		// 2. Ensure cluster state no longer thinks these nodes are deleting
		// 3. Remove it from the Queue's internal data structure
		failedLaunches := lo.Filter(cmd.ReplacementKeys, func(key *NodeClaimKey, _ int) bool {
			return !key.Initialized
		})
		disruptionReplacementNodeClaimFailedCounter.With(map[string]string{
			methodLabel:            cmd.Method,
			consolidationTypeLabel: cmd.ConsolidationType,
		}).Add(float64(len(failedLaunches)))
		q.cluster.UnmarkForDeletion(lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string { return s.ProviderID() })...)
		err = multierr.Append(err, q.RequireNoScheduleTaint(ctx, false, cmd.Candidates...))
		err = multierr.Append(err, cmd.LastError)
		nodeNames := strings.Join(lo.Map(cmd.Candidates, func(s *state.StateNode, _ int) string {
			return s.Name()
		}), ",")
		q.Remove(cmd)
		return reconcile.Result{RequeueAfter: controller.Immediately}, fmt.Errorf("failed to deprovision nodes %s, %w", nodeNames, err)
	}
	// Requeue command if not complete
	return reconcile.Result{RequeueAfter: controller.Immediately}, nil
}

func (q *Queue) Process(ctx context.Context, cmd *Command) error {
	if q.clock.Since(cmd.TimeAdded) > maxRetryDuration {
		return NewCommandExecutionError(fmt.Errorf("reached timeout"))
	}
	// If the time hasn't expired, either wait or terminate.
	if err := q.WaitOrTerminate(ctx, cmd); err != nil {
		// If there was an error, set this as the command's last error so that we can propagate it.
		if !IsCommandExecutionError(err) {
			return fmt.Errorf("got unrecoverable error, %w", err)
		}
		return err
	}
	return nil
}

// Add will launch replacement nodeClaims and add the command to the queue
// Each command added to the queue should already be validated and ready for execution.
func (q *Queue) Add(ctx context.Context, candidates []*state.StateNode, replacements []*scheduling.NodeClaim, reason string,
	delay time.Duration) error {
	providerIDs := lo.Map(candidates, func(s *state.StateNode, _ int) string {
		return s.ProviderID()
	})
	// Cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	if err := q.RequireNoScheduleTaint(ctx, true, candidates...); err != nil {
		return multierr.Append(fmt.Errorf("cordoning nodes, %w", err), q.RequireNoScheduleTaint(ctx, false, candidates...))
	}

	var nodeClaimKeys []nodeclaim.Key
	var err error
	if len(replacements) > 0 {
		if nodeClaimKeys, err = q.launchReplacementNodeClaims(ctx, replacements, reason); err != nil {
			// If we failed to launch the replacement, don't deprovision.  If this is some permanent failure,
			// we don't want to disrupt workloads with no way to provision new nodes for them.
			return multierr.Append(fmt.Errorf("launching replacement nodeclaim, %w", err), q.RequireNoScheduleTaint(ctx, false, candidates...))
		}
	}

	// We have the new nodeclaims created at the API server so mark the old nodeclaims for deletion
	q.cluster.MarkForDeletion(providerIDs...)
	// Wait for a heuristic period of time to handle initial eventual consistency delay with node claim creation.
	// This allows us to properly exit when we detect not found errors caused by ICE errors or the Node initializationTTL.
	cmd := NewCommand(nodeClaimKeys, candidates, reason, q.clock.Now().Add(delay))
	q.RateLimitingInterface.AddAfter(cmd, delay)
	for _, candidate := range candidates {
		q.ProviderIDToCommand[candidate.ProviderID()] = cmd
	}
	return nil
}

// WaitOrTerminate will wait until launched nodeclaims are ready.
// Once the replacements are ready, it will terminate the candidates.
// Will return true if the item in the queue should be re-queued. If a command has
// timed out, this will return false.
// nolint:gocyclo
func (q *Queue) WaitOrTerminate(ctx context.Context, cmd *Command) error {
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
			if apierrors.IsNotFound(err) {
				return NewCommandExecutionError(fmt.Errorf("nodeclaim not found, %w", err))
			}
			waitErrs[i] = fmt.Errorf("getting node claim, %w", err)
			continue
		}
		// We emitted this event when Deprovisioning was blocked on launching/termination.
		// This does not block other forms of deprovisioning, but we should still emit this.
		q.recorder.Publish(disruptionevents.Launching(nodeClaim, cmd.Reason))
		initializedStatus := nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized)
		if !initializedStatus.IsTrue() {
			q.recorder.Publish(disruptionevents.WaitingOnReadiness(nodeClaim))
			waitErrs[i] = fmt.Errorf("node claim not initialized")
			continue
		}
		key.Initialized = true
		// Subtract the last initialization time from the time the command was added to get initialization duration.
		initLength := initializedStatus.LastTransitionTime.Inner.Time.Sub(cmd.TimeAdded).Seconds()
		disruptionReplacementNodeClaimInitializedHistogram.Observe(initLength)
	}
	// If we have any errors, don't continue
	if err := multierr.Combine(waitErrs...); err != nil {
		return fmt.Errorf("waiting for replacement initialization, %w", err)
	}

	// All replacements have been provisioned.
	// All we need to do now is get a successful delete call for each node claim,
	// then the termination controller will handle the eventual deletion of the nodes.
	var multiErr error
	for i := range cmd.Candidates {
		candidate := cmd.Candidates[i]
		q.recorder.Publish(disruptionevents.Terminating(candidate.Node, candidate.NodeClaim, cmd.Reason)...)
		if err := nodeclaim.Delete(ctx, q.kubeClient, candidate.NodeClaim); err != nil {
			if !apierrors.IsNotFound(err) {
				multiErr = multierr.Append(multiErr, err)
			}
		} else {
			nodeclaim.TerminatedCounter(cmd.Candidates[i].NodeClaim, cmd.Reason).Inc()
		}
	}
	// If there were any deletion failures, we should requeue.
	// In the case where we requeue, but the timeout for the command is reached, we'll mark this as a failure.
	if multiErr != nil {
		return fmt.Errorf("terminating nodeclaims, %w", multiErr)
	}
	return nil
}

// CanAdd is a quick check to see if the candidate is already part of a deprovisioning action
func (q *Queue) CanAdd(ids ...string) error {
	var err error
	for _, id := range ids {
		if _, ok := q.ProviderIDToCommand[id]; ok {
			err = multierr.Append(err, fmt.Errorf("candidate is being deprovisioned"))
		}
	}
	return err
}

// Remove fully clears the queue of all references of a hash/command
func (q *Queue) Remove(cmd *Command) {
	// Remove all candidates linked to the command
	for _, candidate := range cmd.Candidates {
		delete(q.ProviderIDToCommand, candidate.ProviderID())
	}
	q.RateLimitingInterface.Forget(cmd)
	q.RateLimitingInterface.Done(cmd)
}

// RequireNoScheduleTaint will add/remove the karpenter.sh/disruption:NoSchedule taint from the candidates.
// This is used to enforce no taints at the beginning of disruption, and
// to add/remove taints while executing a disruption action.
// nolint:gocyclo
func (q *Queue) RequireNoScheduleTaint(ctx context.Context, addTaint bool, nodes ...*state.StateNode) error {
	var multiErr error
	for _, n := range nodes {
		// If the StateNode is Karpenter owned and only has a nodeclaim, or is not owned by
		// Karpenter, thus having no nodeclaim, don't touch the node.
		if n.Node == nil || n.NodeClaim == nil {
			continue
		}
		node := &v1.Node{}
		if err := q.kubeClient.Get(ctx, client.ObjectKey{Name: n.Node.Name}, node); client.IgnoreNotFound(err) != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("getting node, %w", err))
		}
		// If the node already has the taint, continue to the next
		_, hasTaint := lo.Find(node.Spec.Taints, func(taint v1.Taint) bool {
			return v1beta1.IsDisruptingTaint(taint)
		})
		// Node is being deleted, so no need to remove taint as the node will be gone soon.
		// This ensures that the disruption controller doesn't modify taints that the Termination
		// controller is also modifying
		if hasTaint && !node.DeletionTimestamp.IsZero() {
			continue
		}
		stored := node.DeepCopy()
		// If the taint is present and we want to remove the taint, remove it.
		if !addTaint {
			node.Spec.Taints = lo.Reject(node.Spec.Taints, func(taint v1.Taint, _ int) bool {
				return taint.Key == v1beta1.DisruptionTaintKey
			})
			// otherwise, add it.
		} else if addTaint && !hasTaint {
			// If the taint key is present (but with a different value or effect), remove it.
			node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t v1.Taint, _ int) bool {
				return t.Key == v1beta1.DisruptionTaintKey
			})
			node.Spec.Taints = append(node.Spec.Taints, v1beta1.DisruptionNoScheduleTaint)
		}
		if !equality.Semantic.DeepEqual(stored, node) {
			if err := q.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
				multiErr = multierr.Append(multiErr, fmt.Errorf("patching node %s, %w", node.Name, err))
			}
		}
	}
	return multiErr
}

// Reset is used for testing and clears all internal data structures
func (q *Queue) Reset() {
	q.RateLimitingInterface = workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(queueBaseDelay, queueMaxDelay))
	q.ProviderIDToCommand = map[string]*Command{}
}

// launchReplacementNodeClaims will create replacement node claims
func (q *Queue) launchReplacementNodeClaims(ctx context.Context, replacements []*scheduling.NodeClaim, reason string) ([]nodeclaim.Key, error) {
	nodeClaimKeys, err := q.provisioner.CreateNodeClaims(ctx, replacements, provisioning.WithReason(reason))
	if err != nil {
		// uncordon the nodes as the launch may fail (e.g. ICE)
		return nil, fmt.Errorf("creating node claims, %w", err)
	}
	if len(nodeClaimKeys) != len(replacements) {
		// shouldn't ever occur since a partially failed CreateNodeClaims should return an error
		return nil, fmt.Errorf("expected %d nodes, got %d", len(replacements), len(nodeClaimKeys))
	}
	return nodeClaimKeys, nil
}

//// Pop returns a command and its hash and if the queue has shutdown.
//func (q *Queue) Pop() (*Command, bool) {
//	// Get command from queue. This waits until queue is non-empty.
//	item, shutdown := q.RateLimitingInterface.Get()
//	if shutdown {
//		return &Command{}, true
//	}
//	cmd := item.(*Command)
//	return cmd, false
//}

//// ProcessItem will process a command. It will either:
//// 1. Remove all references of the command if the command completes due to timeout or success.
//// 2. Check the command and requeue if it needs to be checked again later.
//func (q *Queue) ProcessItem(ctx context.Context, cmd *Command) {
//	requeue, err := q.Reconcile(ctx, cmd)
//	if !requeue {
//		// If the command timed out, log the last error.
//		if err != nil {
//			// If the command failed, bail on the action.
//			// 1. Emit metrics for launch failures
//			// 2. Ensure cluster state no longer thinks these nodes are deleting
//			// 3. Remove it from the Queue's internal data structure
//			deprovisioningReplacementNodeLaunchFailedCounter.WithLabelValues(cmd.Reason).Inc()
//			q.cluster.UnmarkForDeletion(lo.Map(cmd.candidates, func(s *state.StateNode, _ int) string { return s.ProviderID() })...)
//			err = multierr.Append(err, q.setNodesUnschedulable(ctx, false, lo.Map(cmd.candidates, func(s *state.StateNode, _ int) string { return s.Node.Name })...))
//			logging.FromContext(ctx).Debugf("handling deprovisioning command, %w, for nodes %s", err,
//				strings.Join(lo.Map(cmd.candidates, func(s *state.StateNode, _ int) string {
//					return s.Name()
//				}), ","))
//		}
//		q.Remove(cmd)
//		return
//	}
//	q.RateLimitingInterface.Done(cmd)
//	// Requeue command if not complete
//	q.RateLimitingInterface.AddRateLimited(cmd)
//}

//// Reconcile will check for timeouts, then execute a wait or termination.
//func (q *Queue) Reconcile(ctx context.Context, cmd *Command) (bool, error) {
//	if q.clock.Since(cmd.TimeAdded) > maxRetryDuration {
//		return false, fmt.Errorf("abandoning command, reached timeout, %w", cmd.LastError)
//	}
//	// If the time hasn't expired, either wait or terminate.
//	requeue, err := q.WaitOrTerminate(ctx, cmd)
//	if err != nil {
//		// If there was an error, set this as the command's last error so that we can propagate it.
//		cmd.LastError = err
//		if !retry.IsRecoverable(err) {
//			return false, nil
//		}
//		return requeue, nil
//	}
//	return requeue, nil
//}

//// Start will kick off the queue. This runs asynchronously until Karpenter shuts down.
//func (q *Queue) Start(ctx context.Context) {
//Loop:
//	for {
//		select {
//		case <-ctx.Done():
//			break Loop
//		default:
//			cmd, shutdown := q.Pop()
//			if shutdown {
//				break Loop
//			}
//			q.ProcessItem(ctx, cmd)
//		}
//	}
//	logging.FromContext(ctx).Errorf("deprovisioning queue is broken and has shutdown")
//}
