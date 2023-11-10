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
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/disruption/orchestration"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

type Controller struct {
	Queue         *orchestration.Queue
	kubeClient    client.Client
	cluster       *state.Cluster
	provisioner   *provisioning.Provisioner
	recorder      events.Recorder
	clock         clock.Clock
	cloudProvider cloudprovider.CloudProvider
	methods       []Method
	mu            sync.Mutex
	lastRun       map[string]time.Time
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
			NewEmptyNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder, queue),
			// Attempt to identify multiple NodeClaims that we can consolidate simultaneously to reduce pod churn
			NewMultiNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder, queue),
			// And finally fall back our single NodeClaim consolidation to further reduce cluster cost.
			NewSingleNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder, queue),
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
	if err := state.RequireNoScheduleTaint(ctx, c.kubeClient, false, lo.Filter(c.cluster.Nodes(), func(s *state.StateNode, _ int) bool {
		return c.Queue.CanAdd(s.ProviderID()) != nil
	})...); err != nil {
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
	candidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, disruption.ShouldDisrupt, c.Queue)
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

// executeCommand will do the following, untainting if the step fails.
// 1. Taint candidate nodes
// 2. Spin up replacement nodes
// 3. Add Command to orchestration.Queue to wait to delete the candiates.
func (c *Controller) executeCommand(ctx context.Context, m Method, cmd Command) error {
	disruptionActionsPerformedCounter.With(map[string]string{
		actionLabel:            string(cmd.Action()),
		methodLabel:            m.Type(),
		consolidationTypeLabel: m.ConsolidationType(),
	}).Inc()
	logging.FromContext(ctx).Infof("disrupting via %s %s", m.Type(), cmd)

	stateNodes := lo.Map(cmd.candidates, func(c *Candidate, _ int) *state.StateNode {
		return c.StateNode
	})
	// Cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	if err := state.RequireNoScheduleTaint(ctx, c.kubeClient, true, stateNodes...); err != nil {
		return multierr.Append(fmt.Errorf("cordoning nodes, %w", err), state.RequireNoScheduleTaint(ctx, c.kubeClient, false, stateNodes...))
	}
	providerIDs := lo.Map(cmd.candidates, func(c *Candidate, _ int) string { return c.ProviderID() })
	// We have the new NodeClaims created at the API server so mark the old NodeClaims for deletion
	c.cluster.MarkForDeletion(providerIDs...)

	var nodeClaimKeys []nodeclaim.Key
	var err error
	if len(cmd.replacements) > 0 {
		if nodeClaimKeys, err = c.createReplacementNodeClaims(ctx, m, cmd); err != nil {
			// If we failed to launch the replacement, don't disrupt.  If this is some permanent failure,
			// we don't want to disrupt workloads with no way to provision new nodes for them.
			return multierr.Append(fmt.Errorf("launching replacement nodeclaim, %w", err), state.RequireNoScheduleTaint(ctx, c.kubeClient, false, stateNodes...))
		}
	}
	if err := c.Queue.Add(orchestration.NewCommand(nodeClaimKeys,
		lo.Map(cmd.candidates, func(c *Candidate, _ int) *state.StateNode { return c.StateNode }), c.clock.Now(), m.Type(), m.ConsolidationType())); err != nil {
		c.cluster.UnmarkForDeletion(providerIDs...)
		return fmt.Errorf("adding command to queue, %w", multierr.Append(err, state.RequireNoScheduleTaint(ctx, c.kubeClient, false, stateNodes...)))
	}
	return nil
}

// createReplacementNodeClaims launches replacement NodeClaims
func (c *Controller) createReplacementNodeClaims(ctx context.Context, m Method, cmd Command) ([]nodeclaim.Key, error) {
	reason := fmt.Sprintf("%s/%s", m.Type(), cmd.Action())
	nodeClaimKeys, err := c.provisioner.CreateNodeClaims(ctx, cmd.replacements, provisioning.WithReason(reason))
	if err != nil {
		// untaint the nodes as the launch may fail (e.g. ICE)
		return nil, err
	}
	if len(nodeClaimKeys) != len(cmd.replacements) {
		// shouldn't ever occur since a partially failed CreateNodeClaims should return an error
		return nil, fmt.Errorf("expected %d replacements, got %d", len(cmd.replacements), len(nodeClaimKeys))
	}
	return nodeClaimKeys, nil
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
