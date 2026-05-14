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
	"math"
	"strings"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/option"
	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/singleton"

	"github.com/google/uuid"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// MethodState tracks CFS scheduling state for a Phase 2 disruption method.
// Phase 1 methods (Emptiness, StaticDrift) do not have MethodState entries.
type MethodState struct {
	druntime float64 // disruption runtime consumed; lower = higher scheduling priority
	weight   float64 // higher weight = druntime grows slower = more turns
	active   bool    // whether this method had eligible candidates last cycle
}

type Controller struct {
	queue         *Queue
	kubeClient    client.Client
	cluster       *state.Cluster
	provisioner   *provisioning.Provisioner
	recorder      events.Recorder
	clock         clock.Clock
	cloudProvider cloudprovider.CloudProvider
	methods       []Method
	mu            sync.Mutex
	lastRun       map[string]time.Time
	methodStates  map[string]*MethodState // CFS state keyed by method type name; Phase 2 only
}

// pollingPeriod that we inspect cluster to look for opportunities to disrupt
const pollingPeriod = 10 * time.Second

type ControllerOptions struct {
	methods []Method
}

func WithMethods(methods ...Method) option.Function[ControllerOptions] {
	return func(o *ControllerOptions) {
		o.methods = methods
	}
}

func NewController(clk clock.Clock, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster, queue *Queue, opts ...option.Function[ControllerOptions]) *Controller {

	o := option.Resolve(append([]option.Function[ControllerOptions]{WithMethods(NewMethods(clk, cluster, kubeClient, provisioner, cp, recorder, queue)...)}, opts...)...)
	methodStates := map[string]*MethodState{}
	for _, m := range o.methods {
		if m.Weight() > 0 {
			methodStates[methodKey(m)] = &MethodState{
				weight: m.Weight(),
				active: true,
			}
		}
	}
	return &Controller{
		queue:         queue,
		clock:         clk,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		recorder:      recorder,
		cloudProvider: cp,
		lastRun:       map[string]time.Time{},
		methods:       o.methods,
		methodStates:  methodStates,
	}
}

func NewMethods(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider, recorder events.Recorder, queue *Queue) []Method {
	c := MakeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder, queue)
	return []Method{
		// Delete any empty NodeClaims as there is zero cost in terms of disruption.
		NewEmptiness(c),
		// Terminate and create replacement for drifted NodeClaims in Static NodePool
		NewStaticDrift(cluster, provisioner, cp),
		// Terminate any NodeClaims that have drifted from provisioning specifications, allowing the pods to reschedule.
		NewDrift(kubeClient, cluster, provisioner, recorder, clk),
		// Attempt to identify multiple NodeClaims that we can consolidate simultaneously to reduce pod churn
		NewMultiNodeConsolidation(c),
		// And finally fall back our single NodeClaim consolidation to further reduce cluster cost.
		NewSingleNodeConsolidation(c),
	}
}

func (c *Controller) Name() string {
	return "disruption"
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	// this won't catch if the reconciler loop hangs forever, but it will catch other issues
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
		return reconciler.Result{RequeueAfter: time.Second}, nil
	}

	// Karpenter taints nodes with a karpenter.sh/disruption taint as part of the disruption process while it progresses in memory.
	// If Karpenter restarts or fails with an error during a disruption action, some nodes can be left tainted.
	// Idempotently remove this taint from candidates that are not in the orchestration queue before continuing.
	outdatedNodes := lo.Reject(c.cluster.DeepCopyNodes(), func(s *state.StateNode, _ int) bool {
		return c.queue.HasAny(s.ProviderID()) || s.MarkedForDeletion()
	})
	if err := state.RequireNoScheduleTaint(ctx, c.kubeClient, false, outdatedNodes...); err != nil {
		if errors.IsConflict(err) {
			return reconciler.Result{Requeue: true}, nil
		}
		return reconciler.Result{}, serrors.Wrap(fmt.Errorf("removing taint from nodes, %w", err), "taint", pretty.Taint(v1.DisruptedNoScheduleTaint))
	}
	if err := state.ClearNodeClaimsCondition(ctx, c.kubeClient, v1.ConditionTypeDisruptionReason, outdatedNodes...); err != nil {
		if errors.IsConflict(err) {
			return reconciler.Result{Requeue: true}, nil
		}
		return reconciler.Result{}, serrors.Wrap(fmt.Errorf("removing condition from nodeclaims, %w", err), "condition", v1.ConditionTypeDisruptionReason)
	}

	// Phase 1: Emptiness and StaticDrift always run unconditionally. These are fast, batch
	// operations that are important enough to never be starved.
	anySuccess := false
	for _, m := range c.methods {
		if m.Weight() > 0 {
			continue // Phase 2 method, handled below
		}
		c.recordRun(methodKey(m))
		success, _, err := c.disrupt(ctx, m)
		if err != nil {
			if errors.IsConflict(err) {
				return reconciler.Result{Requeue: true}, nil
			}
			return reconciler.Result{}, serrors.Wrap(fmt.Errorf("disrupting, %w", err), strings.ToLower(string(m.Reason())), "reason")
		}
		if success {
			anySuccess = true
		}
	}

	// Phase 2: CFS scheduling for Drift, MultiNode, and SingleNode. Each method tracks a
	// druntime (disruption runtime consumed). We iterate eligible methods in CFS order
	// (lowest druntime first), stopping when one succeeds. Methods that find no work are
	// charged druntime for the time consumed and removed from contention for this cycle —
	// CFS ensures that even if Drift continuously finds work, Multi and Single accumulate
	// relatively lower druntime and will be picked first whenever they have candidates.
	phase2 := lo.Filter(c.methods, func(m Method, _ int) bool { return m.Weight() > 0 })
	if len(phase2) > 0 {
		budgetsByReason, err := c.computeBudgetMappings(ctx, phase2)
		if err != nil {
			return reconciler.Result{}, fmt.Errorf("computing disruption budgets, %w", err)
		}
		eligible := c.getEligibleMethods(phase2, budgetsByReason)
		for len(eligible) > 0 {
			picked := c.pickLowestDruntime(eligible)
			state := c.methodStates[methodKey(picked)]
			c.recordRun(methodKey(picked))

			start := c.clock.Now()
			var success bool
			if _, isDrift := picked.(*Drift); isDrift {
				// Drift only processes one candidate per ComputeCommands call, so we run it
				// in a time-capped loop to process multiple candidates within its time share.
				success, err = c.disruptWithTimeCap(ctx, picked, pollingPeriod)
			} else {
				success, _, err = c.disrupt(ctx, picked)
			}
			elapsed := c.clock.Since(start).Seconds()

			if err != nil {
				if errors.IsConflict(err) {
					return reconciler.Result{Requeue: true}, nil
				}
				return reconciler.Result{}, serrors.Wrap(fmt.Errorf("disrupting, %w", err), strings.ToLower(string(picked.Reason())), "reason")
			}

			// Always charge druntime regardless of outcome. A method that spends 180s on
			// SimulateScheduling and finds nothing should not immediately get another turn.
			state.druntime += elapsed / state.weight

			if success {
				anySuccess = true
				break // One successful disruption per cycle; let state settle before re-evaluating.
			}

			// No work found: remove from contention for this cycle and try the next method.
			state.active = false
			eligible = lo.Reject(eligible, func(m Method, _ int) bool { return methodKey(m) == methodKey(picked) })
		}
		c.normalizeDruntimes(phase2)
	}

	if anySuccess {
		return reconciler.Result{RequeueAfter: singleton.RequeueImmediately}, nil
	}
	return reconciler.Result{RequeueAfter: pollingPeriod}, nil
}

// disrupt evaluates a single disruption method. reportCandidateMetrics controls whether
// EligibleNodes is updated; callers within a time-cap loop pass false after the first iteration
// to avoid overwriting the initial eligible count with 0 after candidates are enqueued.
// Returns (success, hadReplacements, error): hadReplacements is true when replacement NodeClaims
// were created (non-empty candidate), which the time-cap loop uses to stop early and avoid
// double-booking pods from nodes whose replacements aren't yet in cluster state.
func (c *Controller) disrupt(ctx context.Context, disruption Method, reportCandidateMetrics ...bool) (bool, bool, error) {
	shouldReport := len(reportCandidateMetrics) == 0 || reportCandidateMetrics[0]
	defer metrics.Measure(EvaluationDurationSeconds, map[string]string{
		metrics.ReasonLabel:    strings.ToLower(string(disruption.Reason())),
		ConsolidationTypeLabel: disruption.ConsolidationType(),
	})()
	candidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, disruption.ShouldDisrupt, disruption.Class(), c.queue)
	if err != nil {
		return false, false, fmt.Errorf("determining candidates, %w", err)
	}
	if shouldReport {
		EligibleNodes.Set(float64(len(candidates)), map[string]string{
			metrics.ReasonLabel: strings.ToLower(string(disruption.Reason())),
		})
	}

	// If there are no candidates, move to the next disruption
	if len(candidates) == 0 {
		return false, false, nil
	}
	disruptionBudgetMapping, err := BuildDisruptionBudgetMapping(ctx, c.cluster, c.clock, c.kubeClient, c.cloudProvider, c.recorder, disruption.Reason(), shouldReport)
	if err != nil {
		return false, false, fmt.Errorf("building disruption budgets, %w", err)
	}
	// Determine the disruption action
	cmds, err := disruption.ComputeCommands(ctx, disruptionBudgetMapping, candidates...)
	if err != nil {
		return false, false, fmt.Errorf("computing disruption decision, %w", err)
	}
	cmds = lo.Filter(cmds, func(c Command, _ int) bool { return c.Decision() != NoOpDecision })
	if len(cmds) == 0 {
		return false, false, nil
	}

	errs := make([]error, len(cmds))
	workqueue.ParallelizeUntil(ctx, len(cmds), len(cmds), func(i int) {
		cmd := cmds[i]

		// Assign common fields
		cmd.CreationTimestamp = c.clock.Now()
		cmd.ID = uuid.New()
		cmd.Method = disruption

		// Attempt to disrupt
		if err := c.queue.StartCommand(ctx, &cmd); err != nil {
			errs[i] = fmt.Errorf("disrupting candidates, %w", err)
		}
	})
	if err = multierr.Combine(errs...); err != nil {
		return false, false, fmt.Errorf("disrupting candidates, %w", err)
	}
	hadReplacements := lo.SomeBy(cmds, func(cmd Command) bool { return len(cmd.Replacements) > 0 })
	return true, hadReplacements, nil
}

// methodKey returns the type name used as a key for methodStates.
func methodKey(m Method) string {
	return fmt.Sprintf("%T", m)
}

// computeBudgetMappings computes disruption budget mappings once per Phase 2 cycle, one per
// distinct reason. This avoids calling BuildDisruptionBudgetMapping once per method when
// multiple methods share the same reason (Drift uses Drifted; Multi and Single share Underutilized).
func (c *Controller) computeBudgetMappings(ctx context.Context, methods []Method) (map[v1.DisruptionReason]map[string]int, error) {
	seen := map[v1.DisruptionReason]bool{}
	result := map[v1.DisruptionReason]map[string]int{}
	for _, m := range methods {
		reason := m.Reason()
		if seen[reason] {
			continue
		}
		seen[reason] = true
		mapping, err := BuildDisruptionBudgetMapping(ctx, c.cluster, c.clock, c.kubeClient, c.cloudProvider, c.recorder, reason)
		if err != nil {
			return nil, fmt.Errorf("building disruption budgets for %s, %w", reason, err)
		}
		result[reason] = mapping
	}
	return result, nil
}

// getEligibleMethods returns the Phase 2 methods that have budget available, applying wakeup
// logic when a method transitions from inactive to active.
func (c *Controller) getEligibleMethods(phase2Methods []Method, budgetsByReason map[v1.DisruptionReason]map[string]int) []Method {
	minDT := c.minDruntime(phase2Methods)
	var eligible []Method
	for _, m := range phase2Methods {
		state := c.methodStates[methodKey(m)]
		if c.hasBudgetForMethod(budgetsByReason, m) {
			if !state.active {
				// Wakeup: preserve any large druntime charge from a prior expensive evaluation.
				// Only bump up to current minimum if the method's druntime is below it (which
				// can happen after normalization when the method has been inactive long enough).
				state.druntime = math.Max(state.druntime, minDT)
				state.active = true
			}
			eligible = append(eligible, m)
		} else {
			state.active = false
		}
	}
	return eligible
}

// hasBudgetForMethod returns true if any NodePool has remaining budget for the method's reason.
func (c *Controller) hasBudgetForMethod(budgetsByReason map[v1.DisruptionReason]map[string]int, m Method) bool {
	for _, budget := range budgetsByReason[m.Reason()] {
		if budget > 0 {
			return true
		}
	}
	return false
}

// pickLowestDruntime selects the eligible method with the lowest druntime.
// Ties are broken by order in the slice (earlier = higher priority: Drift > Multi > Single).
func (c *Controller) pickLowestDruntime(eligible []Method) Method {
	picked := eligible[0]
	minDT := c.methodStates[methodKey(picked)].druntime
	for _, m := range eligible[1:] {
		if dt := c.methodStates[methodKey(m)].druntime; dt < minDT {
			minDT = dt
			picked = m
		}
	}
	return picked
}

// disruptWithTimeCap runs a disruption method in a loop until the time budget is exhausted or
// the method finds no more work. Used for Drift, which only processes one candidate per
// ComputeCommands call; the time cap lets it process multiple candidates within its time share.
//
// The loop stops immediately after any disruption that creates replacement NodeClaims (non-empty
// candidate). Without this guard, the next iteration would include pods from the newly-deleting
// node in SimulateScheduling, but the replacement NodeClaim isn't yet in cluster state, causing
// duplicate replacements to be created (double-booking). Empty-node deletes are safe to batch
// because they create no replacements and leave no reschedulable pods.
func (c *Controller) disruptWithTimeCap(ctx context.Context, m Method, budget time.Duration) (bool, error) {
	anySuccess := false
	first := true
	deadline := c.clock.Now().Add(budget)
	for c.clock.Now().Before(deadline) {
		// Only report EligibleNodes on the first iteration. Subsequent calls see fewer
		// candidates because earlier nodes were enqueued, which would falsely overwrite
		// the gauge with a lower count.
		success, hadReplacements, err := c.disrupt(ctx, m, first)
		first = false
		if err != nil {
			return anySuccess, err
		}
		if !success {
			break
		}
		anySuccess = true
		if hadReplacements {
			// A replacement NodeClaim was created but is not yet in cluster state. Continuing
			// the loop would double-book pods from the newly-deleting node on the next simulation.
			break
		}
	}
	return anySuccess, nil
}

// normalizeDruntimes subtracts the minimum druntime from all Phase 2 methods to prevent
// unbounded float64 growth while preserving relative scheduling order.
func (c *Controller) normalizeDruntimes(phase2Methods []Method) {
	minDT := c.minDruntime(phase2Methods)
	if minDT == 0 {
		return
	}
	for _, m := range phase2Methods {
		c.methodStates[methodKey(m)].druntime -= minDT
	}
}

// minDruntime returns the minimum druntime across all Phase 2 methods.
func (c *Controller) minDruntime(phase2Methods []Method) float64 {
	if len(phase2Methods) == 0 {
		return 0
	}
	minDT := math.MaxFloat64
	for _, m := range phase2Methods {
		if dt := c.methodStates[methodKey(m)].druntime; dt < minDT {
			minDT = dt
		}
	}
	return minDT
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
			log.FromContext(ctx).V(1).Info("abnormal time between runs", "name", name, "time_since", timeSince)
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
