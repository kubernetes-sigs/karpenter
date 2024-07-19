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
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	operatorlogging "sigs.k8s.io/karpenter/pkg/operator/logging"
)

type Controller struct {
	queue         *orchestration.Queue
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

func NewController(clk clock.Clock, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster, queue *orchestration.Queue,
) *Controller {
	c := MakeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder, queue)
	return &Controller{
		queue:         queue,
		clock:         clk,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		recorder:      recorder,
		cloudProvider: cp,
		lastRun:       map[string]time.Time{},
		methods: []Method{
			// Terminate any NodeClaims that have drifted from provisioning specifications, allowing the pods to reschedule.
			NewDrift(kubeClient, cluster, provisioner, recorder),
			// Delete any remaining empty NodeClaims as there is zero cost in terms of disruption.  Emptiness and
			// emptyNodeConsolidation are mutually exclusive, only one of these will operate
			NewEmptiness(clk, recorder),
			NewEmptyNodeConsolidation(c),
			// Attempt to identify multiple NodeClaims that we can consolidate simultaneously to reduce pod churn
			NewMultiNodeConsolidation(c),
			// And finally fall back our single NodeClaim consolidation to further reduce cluster cost.
			NewSingleNodeConsolidation(c),
		},
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("disruption").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "disruption")

	// this won't catch if the reconcile loop hangs forever, but it will catch other issues
	c.logAbnormalRuns(ctx)
	defer c.logAbnormalRuns(ctx)
	c.recordRun("disruption-loop")

	// Log if there are any budgets that are misconfigured that weren't caught by validation.
	c.logInvalidBudgets(ctx)

	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// with making any scheduling decision off of our state nodes. Otherwise, we have the potential to make
	// a scheduling decision based on a smaller subset of nodes in our cluster state than actually exist.
	if !c.cluster.Synced(ctx) {
		log.FromContext(ctx).V(1).Info("waiting on cluster sync")
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}

	// Karpenter taints nodes with a karpenter.sh/disruption taint as part of the disruption process
	// while it progresses in memory. If Karpenter restarts during a disruption action, some nodes can be left tainted.
	// Idempotently remove this taint from candidates that are not in the orchestration queue before continuing.
	if err := state.RequireNoScheduleTaint(ctx, c.kubeClient, false, lo.Filter(c.cluster.Nodes(), func(s *state.StateNode, _ int) bool {
		return !c.queue.HasAny(s.ProviderID())
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
			return reconcile.Result{RequeueAfter: singleton.RequeueImmediately}, nil
		}
	}

	// All methods did nothing, so return nothing to do
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

func (c *Controller) disrupt(ctx context.Context, disruption Method) (bool, error) {
	defer metrics.Measure(EvaluationDurationHistogram.With(map[string]string{
		methodLabel:            disruption.Type(),
		consolidationTypeLabel: disruption.ConsolidationType(),
	}))()
	candidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, disruption.ShouldDisrupt, disruption.Class(), c.queue)
	if err != nil {
		return false, fmt.Errorf("determining candidates, %w", err)
	}
	EligibleNodesGauge.With(map[string]string{
		methodLabel:            disruption.Type(),
		consolidationTypeLabel: disruption.ConsolidationType(),
	}).Set(float64(len(candidates)))

	// If there are no candidates, move to the next disruption
	if len(candidates) == 0 {
		return false, nil
	}
	disruptionBudgetMapping, err := BuildDisruptionBudgets(ctx, c.cluster, c.clock, c.kubeClient, c.recorder)
	if err != nil {
		return false, fmt.Errorf("building disruption budgets, %w", err)
	}
	// Determine the disruption action
	cmd, schedulingResults, err := disruption.ComputeCommand(ctx, disruptionBudgetMapping, candidates...)
	if err != nil {
		return false, fmt.Errorf("computing disruption decision, %w", err)
	}
	if cmd.Action() == NoOpAction {
		return false, nil
	}

	// Attempt to disrupt
	if err := c.executeCommand(ctx, disruption, cmd, schedulingResults); err != nil {
		return false, fmt.Errorf("disrupting candidates, %w", err)
	}
	return true, nil
}

// executeCommand will do the following, untainting if the step fails.
// 1. Taint candidate nodes
// 2. Spin up replacement nodes
// 3. Add Command to orchestration.Queue to wait to delete the candiates.
func (c *Controller) executeCommand(ctx context.Context, m Method, cmd Command, schedulingResults scheduling.Results) error {
	commandID := uuid.NewUUID()
	log.FromContext(ctx).WithValues("command-id", commandID).Info(fmt.Sprintf("disrupting via %s %s", m.Type(), cmd))

	stateNodes := lo.Map(cmd.candidates, func(c *Candidate, _ int) *state.StateNode {
		return c.StateNode
	})
	// Cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	if err := state.RequireNoScheduleTaint(ctx, c.kubeClient, true, stateNodes...); err != nil {
		return multierr.Append(fmt.Errorf("tainting nodes (command-id: %s), %w", commandID, err), state.RequireNoScheduleTaint(ctx, c.kubeClient, false, stateNodes...))
	}

	var nodeClaimNames []string
	var err error
	if len(cmd.replacements) > 0 {
		if nodeClaimNames, err = c.createReplacementNodeClaims(ctx, m, cmd); err != nil {
			// If we failed to launch the replacement, don't disrupt.  If this is some permanent failure,
			// we don't want to disrupt workloads with no way to provision new nodes for them.
			return multierr.Append(fmt.Errorf("launching replacement nodeclaim (command-id: %s), %w", commandID, err), state.RequireNoScheduleTaint(ctx, c.kubeClient, false, stateNodes...))
		}
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
	schedulingResults.Record(log.IntoContext(ctx, operatorlogging.NopLogger), c.recorder, c.cluster)

	providerIDs := lo.Map(cmd.candidates, func(c *Candidate, _ int) string { return c.ProviderID() })
	// We have the new NodeClaims created at the API server so mark the old NodeClaims for deletion
	c.cluster.MarkForDeletion(providerIDs...)

	if err := c.queue.Add(orchestration.NewCommand(nodeClaimNames,
		lo.Map(cmd.candidates, func(c *Candidate, _ int) *state.StateNode { return c.StateNode }), commandID, m.Type(), m.ConsolidationType())); err != nil {
		c.cluster.UnmarkForDeletion(providerIDs...)
		return fmt.Errorf("adding command to queue (command-id: %s), %w", commandID, multierr.Append(err, state.RequireNoScheduleTaint(ctx, c.kubeClient, false, stateNodes...)))
	}

	// An action is only performed and pods/nodes are only disrupted after a successful add to the queue
	DecisionsPerformedCounter.With(map[string]string{
		actionLabel:            string(cmd.Action()),
		methodLabel:            m.Type(),
		consolidationTypeLabel: m.ConsolidationType(),
	}).Inc()
	for _, cd := range cmd.candidates {
		PodsDisruptedCounter.With(map[string]string{
			metrics.NodePoolLabel:  cd.nodePool.Name,
			actionLabel:            string(cmd.Action()),
			methodLabel:            m.Type(),
			consolidationTypeLabel: m.ConsolidationType(),
		}).Add(float64(len(cd.reschedulablePods)))
	}
	return nil
}

// createReplacementNodeClaims creates replacement NodeClaims
func (c *Controller) createReplacementNodeClaims(ctx context.Context, m Method, cmd Command) ([]string, error) {
	reason := fmt.Sprintf("%s/%s", m.Type(), cmd.Action())
	nodeClaimNames, err := c.provisioner.CreateNodeClaims(ctx, cmd.replacements, provisioning.WithReason(reason))
	if err != nil {
		return nil, err
	}
	if len(nodeClaimNames) != len(cmd.replacements) {
		// shouldn't ever occur since a partially failed CreateNodeClaims should return an error
		return nil, fmt.Errorf("expected %d replacements, got %d", len(cmd.replacements), len(nodeClaimNames))
	}
	return nodeClaimNames, nil
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
			log.FromContext(ctx).V(1).Info(fmt.Sprintf("abnormal time between runs of %s = %s", name, timeSince))
		}
	}
}

// logInvalidBudgets will log if there are any invalid schedules detected
func (c *Controller) logInvalidBudgets(ctx context.Context) {
	nodePoolList := &v1.NodePoolList{}
	if err := c.kubeClient.List(ctx, nodePoolList); err != nil {
		log.FromContext(ctx).Error(err, "failed listing nodepools")
		return
	}
	var buf bytes.Buffer
	for _, np := range nodePoolList.Items {
		// Use a dummy value of 100 since we only care if this errors.
		if _, err := np.GetAllowedDisruptionsByReason(ctx, c.clock, 100); err != nil {
			fmt.Fprintf(&buf, "invalid disruption budgets in nodepool %s, %s", np.Name, err)
		}
	}
	if buf.Len() > 0 {
		log.FromContext(ctx).Error(errors.New(buf.String()), "detected disruption budget errors")
	}
}
