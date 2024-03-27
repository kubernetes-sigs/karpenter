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

package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/controller"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
)

const (
	queueBaseDelay   = 1 * time.Second
	queueMaxDelay    = 10 * time.Second
	maxRetryDuration = 10 * time.Minute
)

type Command struct {
	Replacements      []Replacement
	candidates        []*state.StateNode
	timeAdded         time.Time // timeAdded is used to track timeouts
	id                types.UID // used for log tracking
	method            string    // used for metrics
	consolidationType string    // used for metrics
	lastError         error
}

// Replacement wraps a NodeClaim name with an initialized field to save on readiness checks and identify
// when a NodeClaim is first initialized for metrics and events.
type Replacement struct {
	name string
	// Use a bool track if a node has already been initialized so we can fire metrics for intialization once.
	// This intentionally does not capture nodes that go initialized then go NotReady after as other pods can
	// schedule to this node as well.
	Initialized bool
}

func (c *Command) Reason() string {
	return fmt.Sprintf("%s/%s", c.method,
		lo.Ternary(len(c.Replacements) > 0, "replace", "delete"))
}

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
	return errors.As(err, &unrecoverableError)
}

type Queue struct {
	workqueue.RateLimitingInterface

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
		RateLimitingInterface: workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(queueBaseDelay, queueMaxDelay)),
		providerIDToCommand:   map[string]*Command{},
		kubeClient:            kubeClient,
		recorder:              recorder,
		cluster:               cluster,
		clock:                 clock,
		provisioner:           provisioner,
	}
	return queue
}

// NewTestingQueue uses a test RateLimitingInterface that will immediately re-queue items.
func NewTestingQueue(kubeClient client.Client, recorder events.Recorder, cluster *state.Cluster, clock clock.Clock,
	provisioner *provisioning.Provisioner,
) *Queue {
	queue := &Queue{
		RateLimitingInterface: &controllertest.Queue{Interface: workqueue.New()},
		providerIDToCommand:   map[string]*Command{},
		kubeClient:            kubeClient,
		recorder:              recorder,
		cluster:               cluster,
		clock:                 clock,
		provisioner:           provisioner,
	}
	return queue
}

// NewCommand creates a command key and adds in initial data for the orchestration queue.
func NewCommand(replacements []string, candidates []*state.StateNode, id types.UID, method, consolidationType string) *Command {
	return &Command{
		Replacements: lo.Map(replacements, func(name string, _ int) Replacement {
			return Replacement{name: name}
		}),
		candidates:        candidates,
		method:            method,
		consolidationType: consolidationType,
		id:                id,
	}
}

func (q *Queue) Name() string {
	return "disruption.queue"
}

func (q *Queue) Builder(_ context.Context, m manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(m)
}

func (q *Queue) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// The queue depth is the number of commands currently being considered.
	// This should not use the RateLimitingInterface.Len() method, as this does not include
	// commands that haven't completed their requeue backoff.
	disruptionQueueDepthGauge.Set(float64(len(lo.Uniq(lo.Values(q.providerIDToCommand)))))

	// Check if the queue is empty. client-go recommends not using this function to gate the subsequent
	// get call, but since we're popping items off the queue synchronously retrying, there should be
	// no synchonization issues.
	if q.Len() == 0 {
		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Get command from queue. This waits until queue is non-empty.
	item, shutdown := q.RateLimitingInterface.Get()
	if shutdown {
		panic("unexpected failure, disruption queue has shut down")
	}
	cmd := item.(*Command)
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("command-id", string(cmd.id)))
	if err := q.waitOrTerminate(ctx, cmd); err != nil {
		// If recoverable, re-queue and try again.
		if !IsUnrecoverableError(err) {
			// store the error that is causing us to fail so we can bubble it up later if this times out.
			cmd.lastError = err
			// mark this item as done processing. This is necessary so that the RLI is able to add the item back in.
			q.RateLimitingInterface.Done(cmd)
			q.RateLimitingInterface.AddRateLimited(cmd)
			return reconcile.Result{RequeueAfter: controller.Immediately}, nil
		}
		// If the command failed, bail on the action.
		// 1. Emit metrics for launch failures
		// 2. Ensure cluster state no longer thinks these nodes are deleting
		// 3. Remove it from the Queue's internal data structure
		failedLaunches := lo.Filter(cmd.Replacements, func(r Replacement, _ int) bool {
			return !r.Initialized
		})
		disruptionReplacementNodeClaimFailedCounter.With(map[string]string{
			methodLabel:            cmd.method,
			consolidationTypeLabel: cmd.consolidationType,
		}).Add(float64(len(failedLaunches)))
		multiErr := multierr.Combine(err, cmd.lastError, state.RequireNoScheduleTaint(ctx, q.kubeClient, false, cmd.candidates...))
		// Log the error
		logging.FromContext(ctx).With("nodes", strings.Join(lo.Map(cmd.candidates, func(s *state.StateNode, _ int) string {
			return s.Name()
		}), ",")).Errorf("failed to disrupt nodes, %s", multiErr)
	}
	// If command is complete, remove command from queue.
	q.Remove(cmd)
	logging.FromContext(ctx).Infof("command succeeded")
	return reconcile.Result{RequeueAfter: controller.Immediately}, nil
}

// waitOrTerminate will wait until launched nodeclaims are ready.
// Once the replacements are ready, it will terminate the candidates.
// Will return true if the item in the queue should be re-queued. If a command has
// timed out, this will return false.
// nolint:gocyclo
func (q *Queue) waitOrTerminate(ctx context.Context, cmd *Command) error {
	if q.clock.Since(cmd.timeAdded) > maxRetryDuration {
		return NewUnrecoverableError(fmt.Errorf("command reached timeout after %s", q.clock.Since(cmd.timeAdded)))
	}
	waitErrs := make([]error, len(cmd.Replacements))
	for i := range cmd.Replacements {
		// If we know the node claim is Initialized, no need to check again.
		if cmd.Replacements[i].Initialized {
			continue
		}
		// Get the nodeclaim
		nodeClaim := &v1beta1.NodeClaim{}
		if err := q.kubeClient.Get(ctx, types.NamespacedName{Name: cmd.Replacements[i].name}, nodeClaim); err != nil {
			// The NodeClaim got deleted after an initial eventual consistency delay
			// This means that there was an ICE error or the Node initializationTTL expired
			// In this case, the error is unrecoverable, so don't requeue.
			if apierrors.IsNotFound(err) && q.clock.Since(cmd.timeAdded) > time.Second*5 {
				return NewUnrecoverableError(fmt.Errorf("replacement was deleted, %w", err))
			}
			waitErrs[i] = fmt.Errorf("getting node claim, %w", err)
			continue
		}
		// We emitted this event when disruption was blocked on launching/termination.
		// This does not block other forms of deprovisioning, but we should still emit this.
		q.recorder.Publish(disruptionevents.Launching(nodeClaim, cmd.Reason()))
		initializedStatus := nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized)
		if !initializedStatus.IsTrue() {
			q.recorder.Publish(disruptionevents.WaitingOnReadiness(nodeClaim))
			waitErrs[i] = fmt.Errorf("nodeclaim %s not initialized", nodeClaim.Name)
			continue
		}
		cmd.Replacements[i].Initialized = true
		// Subtract the last initialization time from the time the command was added to get initialization duration.
		initLength := initializedStatus.LastTransitionTime.Inner.Time.Sub(nodeClaim.CreationTimestamp.Time).Seconds()
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
	for i := range cmd.candidates {
		candidate := cmd.candidates[i]
		q.recorder.Publish(disruptionevents.Terminating(candidate.Node, candidate.NodeClaim, cmd.Reason())...)
		if err := q.kubeClient.Delete(ctx, candidate.NodeClaim); err != nil {
			multiErr = multierr.Append(multiErr, client.IgnoreNotFound(err))
		} else {
			metrics.NodeClaimsTerminatedCounter.With(prometheus.Labels{
				metrics.ReasonLabel:       cmd.method,
				metrics.NodePoolLabel:     cmd.candidates[i].NodeClaim.Labels[v1beta1.NodePoolLabelKey],
				metrics.ZoneLabel:         cmd.candidates[i].NodeClaim.Labels[v1.LabelTopologyZone],
				metrics.ArchLabel:         cmd.candidates[i].NodeClaim.Labels[v1.LabelArchStable],
				metrics.CapacityTypeLabel: cmd.candidates[i].NodeClaim.Labels[v1beta1.CapacityTypeLabelKey],
				metrics.InstanceTypeLabel: cmd.candidates[i].NodeClaim.Labels[v1.LabelInstanceTypeStable],
			}).Inc()
		}
	}
	// If there were any deletion failures, we should requeue.
	// In the case where we requeue, but the timeout for the command is reached, we'll mark this as a failure.
	if multiErr != nil {
		return fmt.Errorf("terminating nodeclaims, %w", multiErr)
	}
	return nil
}

// Add adds commands to the Queue
// Each command added to the queue should already be validated and ready for execution.
func (q *Queue) Add(cmd *Command) error {
	providerIDs := lo.Map(cmd.candidates, func(s *state.StateNode, _ int) string {
		return s.ProviderID()
	})
	// First check if we can add the command.
	if q.HasAny(providerIDs...) {
		return fmt.Errorf("candidate is being disrupted")
	}

	cmd.timeAdded = q.clock.Now()
	q.mu.Lock()
	for _, candidate := range cmd.candidates {
		q.providerIDToCommand[candidate.ProviderID()] = cmd
	}
	q.mu.Unlock()
	q.RateLimitingInterface.Add(cmd)
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

// Remove fully clears the queue of all references of a hash/command
func (q *Queue) Remove(cmd *Command) {
	// mark this item as done processing. This is necessary so that the RLI is able to add the item back in.
	q.RateLimitingInterface.Done(cmd)
	q.RateLimitingInterface.Forget(cmd)
	q.cluster.UnmarkForDeletion(lo.Map(cmd.candidates, func(s *state.StateNode, _ int) string { return s.ProviderID() })...)
	// Remove all candidates linked to the command
	q.mu.Lock()
	for _, candidate := range cmd.candidates {
		delete(q.providerIDToCommand, candidate.ProviderID())
	}
	q.mu.Unlock()
}

// Reset is used for testing and clears all internal data structures
func (q *Queue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.RateLimitingInterface = &controllertest.Queue{Interface: workqueue.New()}
	q.providerIDToCommand = map[string]*Command{}
}

func (q *Queue) IsEmpty() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.providerIDToCommand) == 0
}
