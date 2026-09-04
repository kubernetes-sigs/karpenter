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

	"github.com/awslabs/operatorpkg/option"
	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/singleton"

	"github.com/google/uuid"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
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
	"sigs.k8s.io/karpenter/pkg/state/cost"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

type Controller struct {
	queue         *Queue
	kubeClient    client.Client
	cluster       *state.Cluster
	provisioner   *provisioning.Provisioner
	recorder      events.Recorder
	clock         clock.Clock
	cloudProvider cloudprovider.CloudProvider
	clusterCost   *cost.ClusterCost
	methods       []Method
	mu            sync.Mutex
	lastRun       map[string]time.Time
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
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster, queue *Queue, clusterCost *cost.ClusterCost, opts ...option.Function[ControllerOptions]) *Controller {

	o := option.Resolve(append([]option.Function[ControllerOptions]{WithMethods(NewMethods(clk, cluster, kubeClient, provisioner, cp, recorder, queue)...)}, opts...)...)
	return &Controller{
		queue:         queue,
		clock:         clk,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		recorder:      recorder,
		cloudProvider: cp,
		clusterCost:   clusterCost,
		lastRun:       map[string]time.Time{},
		methods:       o.methods,
	}
}

func NewMethods(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider, recorder events.Recorder, queue *Queue) []Method {
	c := MakeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder, queue)
	return []Method{
		// Delete empty nodes across all consolidation policies (WhenEmpty, WhenEmptyOrUnderutilized, Balanced).
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
	if err := state.ClearNodeClaimsCondition(ctx, c.kubeClient, c.clock, v1.ConditionTypeDisruptionReason, outdatedNodes...); err != nil {
		if errors.IsConflict(err) {
			return reconciler.Result{Requeue: true}, nil
		}
		return reconciler.Result{}, serrors.Wrap(fmt.Errorf("removing condition from nodeclaims, %w", err), "condition", v1.ConditionTypeDisruptionReason)
	}

	// Attempt different disruption methods. We'll only let one method perform an action
	for _, m := range c.methods {
		c.recordRun(fmt.Sprintf("%T", m))
		success, err := c.disrupt(ctx, m)
		if err != nil {
			if errors.IsConflict(err) {
				return reconciler.Result{Requeue: true}, nil
			}
			return reconciler.Result{}, serrors.Wrap(fmt.Errorf("disrupting, %w", err), strings.ToLower(string(m.Reason())), "reason")
		}
		if success {
			return reconciler.Result{RequeueAfter: singleton.RequeueImmediately}, nil
		}
	}

	// All methods did nothing, so return nothing to do
	return reconciler.Result{RequeueAfter: pollingPeriod}, nil
}

func (c *Controller) disrupt(ctx context.Context, disruption Method) (bool, error) {
	passOutcome := PassOutcomeError
	var observation *EvaluationObservation
	var nodePools []string
	defer func() {
		if observation == nil {
			observation = NewEvaluationObservation(disruption, nil)
		}
		observation.Complete(passOutcome)
		if nodePools != nil {
			c.recordLastEvaluated(disruption, nodePools)
		}
	}()
	defer metrics.Measure(EvaluationDurationSeconds, map[string]string{
		metrics.ReasonLabel:    strings.ToLower(string(disruption.Reason())),
		ConsolidationTypeLabel: disruption.ConsolidationType(),
	})()
	candidateSet, nodePoolTotals, err := GetCandidatesWithTotals(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, disruption.ShouldDisrupt, disruption.Class(), c.queue, c.clusterCost)
	if err != nil {
		return false, fmt.Errorf("determining candidates, %w", err)
	}
	nodePools = candidateSet.NodePools
	observation = NewEvaluationObservation(disruption, candidateSet.Eligible)
	ctx = WithEvaluationObservation(ctx, observation)
	c.recordCandidateSnapshots(disruption, candidateSet)
	candidates := candidateSet.Eligible
	EligibleNodes.Set(float64(len(candidates)), map[string]string{
		metrics.ReasonLabel: strings.ToLower(string(disruption.Reason())),
	})

	// If there are no candidates, move to the next disruption
	if len(candidates) == 0 {
		passOutcome = PassOutcomeNoCandidates
		return false, nil
	}
	// Pass precomputed NodePool totals to consolidation methods for balanced scoring
	if setter, ok := disruption.(NodePoolTotalsSetter); ok {
		setter.SetNodePoolTotals(nodePoolTotals)
	}
	disruptionBudgetMapping, err := BuildDisruptionBudgetMapping(ctx, c.cluster, c.clock, c.kubeClient, c.cloudProvider, c.recorder, disruption.Reason())
	if err != nil {
		return false, fmt.Errorf("building disruption budgets, %w", err)
	}
	// Determine the disruption action
	cmds, err := disruption.ComputeCommands(ctx, disruptionBudgetMapping, candidates...)
	if err != nil {
		return false, fmt.Errorf("computing disruption decision, %w", err)
	}
	cmds = lo.Filter(cmds, func(c Command, _ int) bool { return c.Decision() != NoOpDecision })
	if len(cmds) == 0 {
		passOutcome = PassOutcomeNoCommand
		return false, nil
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
			return
		}
		recordSelectedCandidates(disruption, cmd)
		recordSelectedPodPlacements(disruption, cmd)
	})
	if err = multierr.Combine(errs...); err != nil {
		return false, fmt.Errorf("disrupting candidates, %w", err)
	}
	passOutcome = PassOutcomeSelected
	return true, nil
}

func (c *Controller) recordCandidateSnapshots(method Method, candidateSet CandidateSet) {
	baseLabels := methodMetricLabels(method)
	Candidates.DeletePartialMatch(baseLabels)
	OldestEligibleAgeSeconds.DeletePartialMatch(baseLabels)

	possibleByNodePool := lo.CountValuesBy(candidateSet.Possible, func(candidate *Candidate) string {
		return candidate.NodePool.Name
	})
	eligibleByNodePool := lo.CountValuesBy(candidateSet.Eligible, func(candidate *Candidate) string {
		return candidate.NodePool.Name
	})
	for _, nodePool := range candidateSet.NodePools {
		labels := withLabel(baseLabels, metrics.NodePoolLabel, nodePool)
		Candidates.Set(float64(possibleByNodePool[nodePool]), withLabel(labels, stageLabel, CandidateStagePossible))
		Candidates.Set(float64(eligibleByNodePool[nodePool]), withLabel(labels, stageLabel, CandidateStageEligible))
	}

	oldestByNodePool := map[string]time.Time{}
	for _, candidate := range candidateSet.Eligible {
		conditionType := v1.ConditionTypeConsolidatable
		if method.Reason() == v1.DisruptionReasonDrifted {
			conditionType = string(v1.DisruptionReasonDrifted)
		}
		condition := candidate.NodeClaim.StatusConditions().Get(conditionType)
		if condition == nil || condition.LastTransitionTime.IsZero() {
			continue
		}
		nodePool := candidate.NodePool.Name
		if oldest, ok := oldestByNodePool[nodePool]; !ok || condition.LastTransitionTime.Time.Before(oldest) {
			oldestByNodePool[nodePool] = condition.LastTransitionTime.Time
		}
	}
	for nodePool, oldest := range oldestByNodePool {
		age := c.clock.Since(oldest).Seconds()
		if age < 0 {
			age = 0
		}
		OldestEligibleAgeSeconds.Set(age, withLabel(baseLabels, metrics.NodePoolLabel, nodePool))
	}
}

func (c *Controller) recordLastEvaluated(method Method, nodePools []string) {
	baseLabels := methodMetricLabels(method)
	LastEvaluatedTimestampSeconds.DeletePartialMatch(baseLabels)
	for _, nodePool := range nodePools {
		LastEvaluatedTimestampSeconds.Set(float64(c.clock.Now().Unix()), withLabel(baseLabels, metrics.NodePoolLabel, nodePool))
	}
}

func recordSelectedCandidates(method Method, cmd Command) {
	counts := lo.CountValuesBy(cmd.Candidates, func(candidate *Candidate) string {
		return candidate.NodePool.Name
	})
	for nodePool, count := range counts {
		labels := methodMetricLabels(method)
		labels[metrics.NodePoolLabel] = nodePool
		labels[decisionLabel] = string(cmd.Decision())
		SelectedCandidatesTotal.Add(float64(count), labels)
	}
}

func recordSelectedPodPlacements(method Method, cmd Command) {
	candidatePodNodePools := map[string]string{}
	for _, candidate := range cmd.Candidates {
		for _, pod := range candidate.reschedulablePods {
			for key := range podKeys(pod) {
				candidatePodNodePools[key] = candidate.NodePool.Name
			}
		}
	}
	podErrors := sets.New[string]()
	for pod := range cmd.Results.PodErrors {
		podErrors.Insert(podKeys(pod).UnsortedList()...)
	}
	seen := sets.New[string]()
	counts := map[string]map[string]int{}
	record := func(pod *corev1.Pod, destination string) {
		key := podKeys(pod).UnsortedList()
		if len(key) == 0 || seen.Has(key[0]) || podErrors.Has(key[0]) {
			return
		}
		nodePool, ok := candidatePodNodePools[key[0]]
		if !ok {
			return
		}
		seen.Insert(key[0])
		if counts[nodePool] == nil {
			counts[nodePool] = map[string]int{}
		}
		counts[nodePool][destination]++
	}
	for _, node := range cmd.Results.ExistingNodes {
		for _, pod := range node.Pods {
			record(pod, SimulationDestinationExistingNode)
		}
	}
	for _, nodeClaim := range cmd.Results.NewNodeClaims {
		for _, pod := range nodeClaim.Pods {
			record(pod, SimulationDestinationNewNodeClaim)
		}
	}
	for nodePool, byDestination := range counts {
		for destination, count := range byDestination {
			labels := methodMetricLabels(method)
			labels[metrics.NodePoolLabel] = nodePool
			labels[destinationLabel] = destination
			SimulationPodPlacementsTotal.Add(float64(count), labels)
		}
	}
}

func methodMetricLabels(method Method) map[string]string {
	return map[string]string{
		methodLabel:            methodName(method),
		metrics.ReasonLabel:    strings.ToLower(string(method.Reason())),
		ConsolidationTypeLabel: method.ConsolidationType(),
	}
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
