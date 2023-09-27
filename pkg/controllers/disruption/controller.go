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

package disruption

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	disruptionevents "github.com/aws/karpenter-core/pkg/controllers/disruption/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

type Controller struct {
	Queue          *orchestration.Queue
	kubeClient     client.Client
	cluster        *state.Cluster
	provisioner    *provisioning.Provisioner
	recorder       events.Recorder
	clock          clock.Clock
	cloudProvider  cloudprovider.CloudProvider
	deprovisioners []Deprovisioner
	mu             sync.Mutex
	lastRun        map[string]time.Time
}

// pollingPeriod that we inspect cluster to look for opportunities to disrupt
const pollingPeriod = 10 * time.Second

var errCandidateDeleting = fmt.Errorf("candidate is deleting")

func NewController(clk clock.Clock, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster, queue *orchestration.Queue) *Controller {

	return &Controller{
		Queue:         queue,
		clock:         clk,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		recorder:      recorder,
		cloudProvider: cp,
		lastRun:       map[string]time.Time{},
		methods: []Method{
			// Expire any NodeClaims that must be deleted, allowing their pods to potentially land on currently
			NewExpiration(clk, kubeClient, cluster, provisioner, recorder),
			// Terminate any NodeClaims that have drifted from provisioning specifications, allowing the pods to reschedule.
			NewDrift(kubeClient, cluster, provisioner, recorder),
			// Delete any remaining empty NodeClaims as there is zero cost in terms of disruption.  Emptiness and
			// emptyNodeConsolidation are mutually exclusive, only one of these will operate
			NewEmptiness(clk),
			NewEmptyNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
			// Attempt to identify multiple NodeClaims that we can consolidate simultaneously to reduce pod churn
			NewMultiNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
			// And finally fall back our single NodeClaim consolidation to further reduce cluster cost.
			NewSingleNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
		},
	}
}

func (c *Controller) Name() string {
	return "disruption"
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(m)
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// this won't catch if the reconcile loop hangs forever, but it will catch other issues
	c.logAbnormalRuns(ctx)
	defer c.logAbnormalRuns(ctx)
	c.recordRun("disruption-loop")

	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// with making any scheduling decision off of our state nodes. Otherwise, we have the potential to make
	// a scheduling decision based on a smaller subset of nodes in our cluster state than actually exist.
	if !c.cluster.Synced(ctx) {
		logging.FromContext(ctx).Debugf("waiting on cluster sync")
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}

	// Karpenter taints nodes with a karpenter.sh/disruption taint as part of the disruption process
	// while it progresses in memory. If Karpenter restarts during a disruption action, some nodes can be left tainted.
	// Idempotently remove this taint from candidates before continuing.
	if err := c.requireNoScheduleTaint(ctx, false, c.cluster.Nodes()...); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing taint from nodes, %w", err)
	}

	// Attempt different disruption methods. We'll only let one method perform an action
	for _, m := range c.methods {
		c.recordRun(fmt.Sprintf("%T", m))
		success, err := c.disrupt(ctx, m)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("disrupting via %q, %w", m.Type(), err)
		}
		if success {
			return reconcile.Result{RequeueAfter: controller.Immediately}, nil
		}
	}

	// All methods did nothing, so return nothing to do
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

func (c *Controller) disrupt(ctx context.Context, disruption Method) (bool, error) {
	defer metrics.Measure(disruptionEvaluationDurationHistogram.With(map[string]string{
		methodLabel:            disruption.Type(),
		consolidationTypeLabel: disruption.ConsolidationType(),
	}))()
	candidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, disruption.ShouldDisrupt)
	if err != nil {
		return false, fmt.Errorf("determining candidates, %w", err)
	}
	// If there are no candidates, move to the next disruption
	if len(candidates) == 0 {
		return false, nil
	}

	// Determine the disruption action
	cmd, err := disruption.ComputeCommand(ctx, candidates...)
	if err != nil {
		return false, fmt.Errorf("computing disruption decision, %w", err)
	}
	if cmd.Action() == NoOpAction {
		return false, nil
	}

	// Attempt to disrupt
	if err := c.executeCommand(ctx, disruption, cmd); err != nil {
		return false, fmt.Errorf("disrupting candidates, %w", err)
	}

	return true, nil
}

func (c *Controller) executeCommand(ctx context.Context, m Method, cmd Command) error {
	disruptionActionsPerformedCounter.With(map[string]string{
		actionLabel:            string(cmd.Action()),
		methodLabel:            m.Type(),
		consolidationTypeLabel: m.ConsolidationType(),
	}).Inc()
	logging.FromContext(ctx).Infof("disrupting via %s %s", m.Type(), cmd)

	reason := fmt.Sprintf("%s/%s", m.Type(), cmd.Action())
	if cmd.Action() == ReplaceAction {
		if err := c.launchReplacementNodeClaims(ctx, m, cmd); err != nil {
			// If we failed to launch the replacement, don't disrupt.  If this is some permanent failure,
			// we don't want to disrupt workloads with no way to provision new NodeClaims for them.
			return fmt.Errorf("launching replacement, %w", err)
		}
	}

	for _, candidate := range cmd.candidates {
		c.recorder.Publish(disruptionevents.Terminating(candidate.Node, candidate.NodeClaim, reason)...)

	// We have the new NodeClaims created at the API server so mark the old NodeClaims for deletion
	c.cluster.MarkForDeletion(candidateProviderIDs...)

	if err := c.Queue.Add(orchestration.CommandItem{
		ReplacementKeys: nodeClaimKeys,
		Candidates:      lo.Map(cmd.Candidates, func(c *Candidate, _ int) *state.StateNode { return c.StateNode }),
		Reason:          reason,
	}); err != nil {
		c.cluster.UnmarkForDeletion(candidateProviderIDs...)
		return fmt.Errorf("adding command to queue, %w", err)
	}
	return nil
}

// launchReplacementNodeClaims launches replacement NodeClaims and blocks until it is ready
// nolint:gocyclo
func (c *Controller) launchReplacementNodeClaims(ctx context.Context, m Method, cmd Command) error {
	reason := fmt.Sprintf("%s/%s", m.Type(), cmd.Action())
	defer metrics.Measure(disruptionReplacementNodeClaimInitializedHistogram)()

	stateNodes := lo.Map(cmd.candidates, func(c *Candidate, _ int) *state.StateNode { return c.StateNode })

	// taint the candidate nodes before we launch the replacements to prevent new pods from scheduling to the candidate nodes
	if err := c.requireNoScheduleTaint(ctx, true, stateNodes...); err != nil {
		return fmt.Errorf("cordoning nodes, %w", err)
	}

	nodeClaimKeys, err := c.provisioner.CreateNodeClaims(ctx, cmd.replacements, provisioning.WithReason(reason))
	if err != nil {
		// untaint the nodes as the launch may fail (e.g. ICE)
		err = multierr.Append(err, c.requireNoScheduleTaint(ctx, false, stateNodes...))
		return err
	}
	if len(nodeClaimKeys) != len(cmd.replacements) {
		// shouldn't ever occur since a partially failed CreateNodeClaims should return an error
		return fmt.Errorf("expected %d replacements, got %d", len(cmd.replacements), len(nodeClaimKeys))
	}

	candidateProviderIDs := lo.Map(cmd.candidates, func(c *Candidate, _ int) string { return c.ProviderID() })
	// We have the new NodeClaims created at the API server so mark the old NodeClaims for deletion
	c.cluster.MarkForDeletion(candidateProviderIDs...)

	errs := make([]error, len(nodeClaimKeys))
	workqueue.ParallelizeUntil(ctx, len(nodeClaimKeys), len(nodeClaimKeys), func(i int) {
		// NodeClaim never became ready or the NodeClaims that we tried to launch got Insufficient Capacity or some
		// other transient error
		if err := c.waitForReadiness(ctx, nodeClaimKeys[i], reason); err != nil {
			disruptionReplacementNodeClaimFailedCounter.With(map[string]string{
				methodLabel:            m.Type(),
				consolidationTypeLabel: m.ConsolidationType(),
			}).Inc()
			errs[i] = err
		}
	})
	if err = multierr.Combine(errs...); err != nil {
		c.cluster.UnmarkForDeletion(candidateProviderIDs...)
		return multierr.Combine(c.requireNoScheduleTaint(ctx, false, stateNodes...),
			fmt.Errorf("timed out checking node readiness, %w", err))
	}
	return nil
}

// TODO @njtran: Allow to bypass this check for certain methods
func (c *Controller) waitForReadiness(ctx context.Context, key nodeclaimutil.Key, reason string) error {
	// Wait for the NodeClaim to be initialized
	var once sync.Once
	pollStart := time.Now()
	return retry.Do(func() error {
		nodeClaim, err := nodeclaimutil.Get(ctx, c.kubeClient, key)
		if err != nil {
			// The NodeClaim got deleted after an initial eventual consistency delay
			// This means that there was an ICE error or the Node initializationTTL expired
			if errors.IsNotFound(err) && c.clock.Since(pollStart) > time.Second*5 {
				return retry.Unrecoverable(fmt.Errorf("getting %s, %w", lo.Ternary(key.IsMachine, "machine", "nodeclaim"), err))
			}
			return fmt.Errorf("getting %s, %w", lo.Ternary(key.IsMachine, "machine", "nodeclaim"), err)
		}
		once.Do(func() {
			c.recorder.Publish(disruptionevents.Launching(nodeClaim, reason))
		})
		if !nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized).IsTrue() {
			// make the user aware of why disruption is paused
			c.recorder.Publish(disruptionevents.WaitingOnReadiness(nodeClaim))
			return fmt.Errorf("node is not initialized")
		}
		return nil
	}, waitRetryOptions(ctx)...)
}

// waitForDeletion waits for the specified NodeClaim to be removed from the API server. This deletion can take some period
// of time if there are PDBs that govern pods on the node as we need to wait until the NodeClaim drains before
// it's actually deleted.
func (c *Controller) waitForDeletion(ctx context.Context, nodeClaim *v1beta1.NodeClaim) {
	if err := retry.Do(func() error {
		nc, nerr := nodeclaimutil.Get(ctx, c.kubeClient, nodeclaimutil.Key{Name: nodeClaim.Name, IsMachine: nodeClaim.IsMachine})
		// We expect the not found error, at which point we know the NodeClaim is deleted.
		if errors.IsNotFound(nerr) {
			return nil
		}
		// make the user aware of why disruption is paused
		c.recorder.Publish(disruptionevents.WaitingOnDeletion(nc))
		if nerr != nil {
			return fmt.Errorf("expected to be not found, %w", nerr)
		}
		// the NodeClaim still exists
		return fmt.Errorf("expected node to be not found")
	}, waitRetryOptions(ctx)...,
	); err != nil {
		logging.FromContext(ctx).Errorf("Waiting on node deletion, %s", err)
	}
	return nodeClaimKeys, nil
}

// requireNoScheduleTaint will add/remove the karpenter.sh/disruption:NoSchedule taint from the candidates.
// This is used to enforce no taints at the beginning of disruption, and
// to add/remove taints while executing a disruption action.
// nolint:gocyclo
func (c *Controller) requireNoScheduleTaint(ctx context.Context, addTaint bool, nodes ...*state.StateNode) error {
	var multiErr error
	for _, n := range nodes {
		// If the StateNode is Karpenter owned and only has a nodeclaim, or is not owned by
		// Karpenter, thus having no nodeclaim, don't touch the node.
		if n.Node == nil || n.NodeClaim == nil {
			continue
		}
		node := &v1.Node{}
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: n.Node.Name}, node); client.IgnoreNotFound(err) != nil {
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
			if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
				multiErr = multierr.Append(multiErr, fmt.Errorf("patching node %s, %w", node.Name, err))
			}
		}
	}
	return multiErr
}

func (c *Controller) recordRun(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRun[s] = c.clock.Now()
}

func (c *Controller) logAbnormalRuns(ctx context.Context) {
	const AbnormalTimeLimit = 15 * time.Minute
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, runTime := range c.lastRun {
		if timeSince := c.clock.Since(runTime); timeSince > AbnormalTimeLimit {
			logging.FromContext(ctx).Debugf("abnormal time between runs of %s = %s", name, timeSince)
		}
	}
}
