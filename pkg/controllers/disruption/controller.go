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
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	operatorlogging "sigs.k8s.io/karpenter/pkg/operator/logging"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
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
			// Delete any empty NodeClaims as there is zero cost in terms of disruption.
			NewEmptiness(c),
			// Terminate any NodeClaims that have drifted from provisioning specifications, allowing the pods to reschedule.
			NewDrift(kubeClient, cluster, provisioner, recorder),
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
	// Only validate the first reason, since CEL validation will catch invalid disruption reasons
	c.logInvalidBudgets(ctx)

	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// with making any scheduling decision off of our state nodes. Otherwise, we have the potential to make
	// a scheduling decision based on a smaller subset of nodes in our cluster state than actually exist.
	if !c.cluster.Synced(ctx) {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}

	// Karpenter taints nodes with a karpenter.sh/disruption taint as part of the disruption process while it progresses in memory.
	// If Karpenter restarts or fails with an error during a disruption action, some nodes can be left tainted.
	// Idempotently remove this taint from candidates that are not in the orchestration queue before continuing.
	outdatedNodes := lo.Filter(c.cluster.Nodes(), func(s *state.StateNode, _ int) bool {
		return !c.queue.HasAny(s.ProviderID()) && !s.Deleted()
	})
	c.cluster.UnmarkForDeletion(lo.Map(outdatedNodes, func(s *state.StateNode, _ int) string { return s.ProviderID() })...)
	if err := state.RequireNoScheduleTaint(ctx, c.kubeClient, false, outdatedNodes...); err != nil {
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, serrors.Wrap(fmt.Errorf("removing taint from nodes, %w", err), "taint", pretty.Taint(v1.DisruptedNoScheduleTaint))
	}
	if err := state.ClearNodeClaimsCondition(ctx, c.kubeClient, v1.ConditionTypeDisruptionReason, outdatedNodes...); err != nil {
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, serrors.Wrap(fmt.Errorf("removing condition from nodeclaims, %w", err), "condition", v1.ConditionTypeDisruptionReason)
	}

	// Attempt different disruption methods. We'll only let one method perform an action
	for _, m := range c.methods {
		c.recordRun(fmt.Sprintf("%T", m))
		success, err := c.disrupt(ctx, m)
		if err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, serrors.Wrap(fmt.Errorf("disrupting, %w", err), strings.ToLower(string(m.Reason())), "reason")
		}
		if success {
			return reconcile.Result{RequeueAfter: singleton.RequeueImmediately}, nil
		}
	}

	// All methods did nothing, so return nothing to do
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

func (c *Controller) disrupt(ctx context.Context, disruption Method) (bool, error) {
	defer metrics.Measure(EvaluationDurationSeconds, map[string]string{
		metrics.ReasonLabel:    strings.ToLower(string(disruption.Reason())),
		ConsolidationTypeLabel: disruption.ConsolidationType(),
	})()
	candidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, disruption.ShouldDisrupt, disruption.Class(), c.queue)
	if err != nil {
		return false, fmt.Errorf("determining candidates, %w", err)
	}
	EligibleNodes.Set(float64(len(candidates)), map[string]string{
		metrics.ReasonLabel: strings.ToLower(string(disruption.Reason())),
	})

	// If there are no candidates, move to the next disruption
	if len(candidates) == 0 {
		return false, nil
	}
	disruptionBudgetMapping, err := BuildDisruptionBudgetMapping(ctx, c.cluster, c.clock, c.kubeClient, c.cloudProvider, c.recorder, disruption.Reason())
	if err != nil {
		return false, fmt.Errorf("building disruption budgets, %w", err)
	}
	// Determine the disruption action
	cmd, schedulingResults, err := disruption.ComputeCommand(ctx, disruptionBudgetMapping, candidates...)
	if err != nil {
		return false, fmt.Errorf("computing disruption decision, %w", err)
	}
	if cmd.Decision() == NoOpDecision {
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
	log.FromContext(ctx).WithValues(append([]any{"command-id", string(commandID), "reason", strings.ToLower(string(m.Reason()))}, cmd.LogValues()...)...).Info("disrupting node(s)")

	// Cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	markedCandidates, markDisruptedErr := c.MarkDisrupted(ctx, m, cmd.candidates...)
	// If we get a failure marking some nodes as disrupted, if we are launching replacements, we shouldn't continue
	// with disrupting the candidates. If it's just a delete operation, we can proceed
	if markDisruptedErr != nil && len(cmd.replacements) > 0 {
		return serrors.Wrap(fmt.Errorf("marking disrupted, %w", markDisruptedErr), "command-id", commandID)
	}

	nodeClaimNames, err := c.createReplacementNodeClaims(ctx, m, cmd)
	if err != nil {
		// If we failed to launch the replacement, don't disrupt.  If this is some permanent failure,
		// we don't want to disrupt workloads with no way to provision new nodes for them.
		return serrors.Wrap(fmt.Errorf("launching replacement nodeclaim, %w", err), "command-id", commandID)
	}

	// IMPORTANT
	// We must MarkForDeletion AFTER we launch the replacements and not before
	// The reason for this is to avoid producing double-launches
	// If we MarkForDeletion before we create replacements, it's possible for the provisioner
	// to recognize that it needs to launch capacity for terminating pods, causing us to launch
	// capacity for these pods twice instead of just once
	c.cluster.MarkForDeletion(lo.Map(cmd.candidates, func(c *Candidate, _ int) string { return c.ProviderID() })...)

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

	stateNodes := lo.Map(markedCandidates, func(c *Candidate, _ int) *state.StateNode { return c.StateNode })
	if err = c.queue.Add(orchestration.NewCommand(nodeClaimNames, stateNodes, commandID, m.Reason(), m.ConsolidationType())); err != nil {
		return multierr.Append(markDisruptedErr, serrors.Wrap(fmt.Errorf("adding command to queue, %w", err), "command-id", commandID))
	}
	// An action is only performed and pods/nodes are only disrupted after a successful add to the queue
	DecisionsPerformedTotal.Inc(map[string]string{
		decisionLabel:          string(cmd.Decision()),
		metrics.ReasonLabel:    strings.ToLower(string(m.Reason())),
		ConsolidationTypeLabel: m.ConsolidationType(),
	})
	return markDisruptedErr
}

// createReplacementNodeClaims creates replacement NodeClaims
func (c *Controller) createReplacementNodeClaims(ctx context.Context, m Method, cmd Command) ([]string, error) {
	nodeClaimNames, err := c.provisioner.CreateNodeClaims(ctx, cmd.replacements, provisioning.WithReason(strings.ToLower(string(m.Reason()))))
	if err != nil {
		return nil, err
	}
	if len(nodeClaimNames) != len(cmd.replacements) {
		// shouldn't ever occur since a partially failed CreateNodeClaims should return an error
		return nil, serrors.Wrap(fmt.Errorf("expected replacement count did not equal actual replacement count"), "expected-count", len(cmd.replacements), "actual-count", len(nodeClaimNames))
	}
	return nodeClaimNames, nil
}

// MarkDisrupted taints the node and adds the Disrupted condition to the NodeClaim for a candidate that is about to be disrupted
func (c *Controller) MarkDisrupted(ctx context.Context, m Method, candidates ...*Candidate) ([]*Candidate, error) {
	errs := make([]error, len(candidates))
	workqueue.ParallelizeUntil(ctx, len(candidates), len(candidates), func(i int) {
		if err := state.RequireNoScheduleTaint(ctx, c.kubeClient, true, candidates[i].StateNode); err != nil {
			errs[i] = serrors.Wrap(fmt.Errorf("tainting nodes, %w", err), "taint", pretty.Taint(v1.DisruptedNoScheduleTaint))
			return
		}
		// refresh nodeclaim before updating status
		nodeClaim := &v1.NodeClaim{}
		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return client.IgnoreNotFound(err) != nil }, func() error {
			return c.kubeClient.Get(ctx, client.ObjectKeyFromObject(candidates[i].NodeClaim), nodeClaim)
		}); err != nil {
			errs[i] = client.IgnoreNotFound(err)
			return
		}
		stored := nodeClaim.DeepCopy()
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeDisruptionReason, string(m.Reason()), string(m.Reason()))
		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return client.IgnoreNotFound(err) != nil }, func() error { return c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored)) }); err != nil {
			errs[i] = client.IgnoreNotFound(err)
			return
		}
	})
	var markedCandidates []*Candidate
	for i := range errs {
		if errs[i] != nil {
			continue
		}
		markedCandidates = append(markedCandidates, candidates[i])
	}
	return markedCandidates, multierr.Combine(errs...)
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
	nps, err := nodepoolutils.ListManaged(ctx, c.kubeClient, c.cloudProvider)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed listing nodepools")
		return
	}
	var buf bytes.Buffer
	for _, np := range nps {
		// Use a dummy value of 100 since we only care if this errors.
		for _, method := range c.methods {
			if _, err := np.GetAllowedDisruptionsByReason(c.clock, 100, method.Reason()); err != nil {
				fmt.Fprintf(&buf, "invalid disruption budgets in nodepool %s, %s", np.Name, err)
				break // Prevent duplicate error message
			}
		}
	}
	if buf.Len() > 0 {
		log.FromContext(ctx).Error(stderrors.New(buf.String()), "detected disruption budget errors")
	}
}
