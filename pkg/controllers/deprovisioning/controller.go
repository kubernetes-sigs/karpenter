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

package deprovisioning

import (
	"context"
	"fmt"
	"sync"
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
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

// Controller is the deprovisioning controller.
type Controller struct {
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

// pollingPeriod that we inspect cluster to look for opportunities to deprovision
const pollingPeriod = 10 * time.Second

var errCandidateDeleting = fmt.Errorf("candidate is deleting")

// waitRetryOptions are the retry options used when waiting on a NodeClaim to become ready or to be deleted
// readiness can take some time as the node needs to come up, have any daemonset extended resoruce plugins register, etc.
// deletion can take some time in the case of restrictive PDBs that throttle the rate at which the node is drained
func waitRetryOptions(ctx context.Context) []retry.Option {
	return []retry.Option{
		retry.Context(ctx),
		retry.Delay(2 * time.Second),
		retry.LastErrorOnly(true),
		retry.Attempts(60),
		retry.MaxDelay(10 * time.Second), // 22 + (60-5)*10 =~ 9.5 minutes in total
	}
}

func NewController(clk clock.Clock, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster) *Controller {

	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		recorder:      recorder,
		cloudProvider: cp,
		lastRun:       map[string]time.Time{},
		deprovisioners: []Deprovisioner{
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
	return "deprovisioning"
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(m)
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// this won't catch if the reconcile loop hangs forever, but it will catch other issues
	c.logAbnormalRuns(ctx)
	defer c.logAbnormalRuns(ctx)
	c.recordRun("deprovisioning-loop")

	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// with making any scheduling decision off of our state nodes. Otherwise, we have the potential to make
	// a scheduling decision based on a smaller subset of nodes in our cluster state than actually exist.
	if !c.cluster.Synced(ctx) {
		logging.FromContext(ctx).Debugf("waiting on cluster sync")
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}

	// Karpenter taints nodes with a karpenter.sh/disrupting taint as part of the deprovisioning process
	// while it progresses in memory. If Karpenter restarts during a deprovisioning action, some nodes can be left tainted.
	// Idempotently remove this taint from candidates before continuing.
	if err := c.requireDisruptingNoScheduleTaint(ctx, false, c.cluster.Nodes()...); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing disruption taint from nodes, %w", err)
	}

	// Attempt different deprovisioning methods. We'll only let one method perform an action
	for _, d := range c.deprovisioners {
		c.recordRun(fmt.Sprintf("%T", d))
		success, err := c.deprovision(ctx, d)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("deprovisioning via %q, %w", d, err)
		}
		if success {
			return reconcile.Result{RequeueAfter: controller.Immediately}, nil
		}
	}

	// All deprovisioners did nothing, so return nothing to do
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

func (c *Controller) deprovision(ctx context.Context, deprovisioner Deprovisioner) (bool, error) {
	defer metrics.Measure(deprovisioningDurationHistogram.WithLabelValues(deprovisioner.String()))()
	candidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, deprovisioner.ShouldDeprovision)
	if err != nil {
		return false, fmt.Errorf("determining candidates, %w", err)
	}
	// If there are no candidates, move to the next deprovisioner
	if len(candidates) == 0 {
		return false, nil
	}

	// Determine the deprovisioning action
	cmd, err := deprovisioner.ComputeCommand(ctx, candidates...)
	if err != nil {
		return false, fmt.Errorf("computing deprovisioning decision, %w", err)
	}
	if cmd.Action() == NoOpAction {
		return false, nil
	}

	// Attempt to deprovision
	if err := c.executeCommand(ctx, deprovisioner, cmd); err != nil {
		return false, fmt.Errorf("deprovisioning candidates, %w", err)
	}

	return true, nil
}

func (c *Controller) executeCommand(ctx context.Context, d Deprovisioner, command Command) error {
	deprovisioningActionsPerformedCounter.With(map[string]string{
		// TODO: make this just command.Action() since we've added the deprovisioner as its own label.
		actionLabel:        fmt.Sprintf("%s/%s", d, command.Action()),
		deprovisionerLabel: d.String(),
	}).Inc()
	logging.FromContext(ctx).Infof("deprovisioning via %s %s", d, command)

	reason := fmt.Sprintf("%s/%s", d, command.Action())
	if command.Action() == ReplaceAction {
		if err := c.launchReplacementNodeClaims(ctx, command, reason); err != nil {
			// If we failed to launch the replacement, don't deprovision.  If this is some permanent failure,
			// we don't want to disrupt workloads with no way to provision new NodeClaims for them.
			return fmt.Errorf("launching replacement, %w", err)
		}
	}

	for _, candidate := range command.candidates {
		c.recorder.Publish(deprovisioningevents.Terminating(candidate.Node, candidate.NodeClaim, reason)...)

		if err := nodeclaimutil.Delete(ctx, c.kubeClient, candidate.NodeClaim); err != nil {
			if !errors.IsNotFound(err) {
				logging.FromContext(ctx).Errorf("terminating, %s", err)
			}
			continue
		}
		nodeclaimutil.TerminatedCounter(candidate.NodeClaim, reason).Inc()
	}

	// We wait for NodeClaims to delete to ensure we don't start another round of deprovisioning until this node is fully
	// deleted.
	for _, oldCandidate := range command.candidates {
		c.waitForDeletion(ctx, oldCandidate.NodeClaim)
	}
	return nil
}

// launchReplacementNodeClaims launches replacement NodeClaims and blocks until it is ready
// nolint:gocyclo
func (c *Controller) launchReplacementNodeClaims(ctx context.Context, action Command, reason string) error {
	defer metrics.Measure(deprovisioningReplacementNodeInitializedHistogram)()

	stateNodes := lo.Map(action.candidates, func(c *Candidate, _ int) *state.StateNode { return c.StateNode })

	// taint the candidate nodes before we launch the replacements to prevent new pods from scheduling to the candidate nodes
	if err := c.requireDisruptingNoScheduleTaint(ctx, true, stateNodes...); err != nil {
		return fmt.Errorf("cordoning nodes, %w", err)
	}

	nodeClaimKeys, err := c.provisioner.CreateNodeClaims(ctx, action.replacements, provisioning.WithReason(reason))
	if err != nil {
		// untaint the nodes as the launch may fail (e.g. ICE)
		err = multierr.Append(err, c.requireDisruptingNoScheduleTaint(ctx, false, stateNodes...))
		return err
	}
	if len(nodeClaimKeys) != len(action.replacements) {
		// shouldn't ever occur since a partially failed CreateNodeClaims should return an error
		return fmt.Errorf("expected %d replacements, got %d", len(action.replacements), len(nodeClaimKeys))
	}

	errs := make([]error, len(nodeClaimKeys))
	workqueue.ParallelizeUntil(ctx, len(nodeClaimKeys), len(nodeClaimKeys), func(i int) {
		// NodeClaim never became ready or the NodeClaims that we tried to launch got Insufficient Capacity or some
		// other transient error
		if err := c.waitForReadiness(ctx, nodeClaimKeys[i], reason); err != nil {
			deprovisioningReplacementNodeLaunchFailedCounter.WithLabelValues(reason).Inc()
			errs[i] = err
		}
	})
	if err = multierr.Combine(errs...); err != nil {
		return multierr.Combine(c.requireDisruptingNoScheduleTaint(ctx, false, stateNodes...),
			fmt.Errorf("timed out checking machine readiness, %w", err))
	}
	return nil
}

// TODO @njtran: Allow to bypass this check for certain deprovisioners
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
			c.recorder.Publish(deprovisioningevents.Launching(nodeClaim, reason))
		})
		if !nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized).IsTrue() {
			// make the user aware of why deprovisioning is paused
			c.recorder.Publish(deprovisioningevents.WaitingOnReadiness(nodeClaim))
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
		// make the user aware of why deprovisioning is paused
		c.recorder.Publish(deprovisioningevents.WaitingOnDeletion(nc))
		if nerr != nil {
			return fmt.Errorf("expected to be not found, %w", nerr)
		}
		// the NodeClaim still exists
		return fmt.Errorf("expected node to be not found")
	}, waitRetryOptions(ctx)...,
	); err != nil {
		logging.FromContext(ctx).Errorf("Waiting on node deletion, %s", err)
	}
}

// requireDisruptingNoScheduleTaint will either add/remove the karpenter.sh/disrupting taint from the candidates.
// This is used to enforce no taints at the beginning of deprovisioning, and
// to add/remove taints while executing a deprovisioning action.
func (c *Controller) requireDisruptingNoScheduleTaint(ctx context.Context, addTaint bool, nodes ...*state.StateNode) error {
	var multiErr error
	for _, n := range nodes {
		node := &v1.Node{}
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: n.Name()}, node); client.IgnoreNotFound(err) != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("getting node, %w", err))
		}

		// If the node already has the taint, continue to the next
		_, ok := lo.Find(node.Spec.Taints, func(taint v1.Taint) bool {
			return v1beta1.DisruptingNoScheduleTaint.MatchTaint(&taint)
		})

		// node is being deleted, so no need to remove taint as the node will be gone soon
		if ok && !node.DeletionTimestamp.IsZero() {
			continue
		}
		// If the taint is how we want it, do nothing.
		if ok == addTaint {
			continue
		}
		stored := node.DeepCopy()
		if ok == !addTaint {
			node.Spec.Taints = lo.Reject(node.Spec.Taints, func(taint v1.Taint, _ int) bool {
				return v1beta1.DisruptingNoScheduleTaint.MatchTaint(&taint)
			})
		} else {
			node.Spec.Taints = append(node.Spec.Taints, v1beta1.DisruptingNoScheduleTaint)
		}
		if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("patching node %s, %w", node.Name, err))
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
